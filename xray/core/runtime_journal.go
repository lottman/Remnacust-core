package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"google.golang.org/protobuf/proto"
)

const RuntimeJournalLimit = 16 * 1024 * 1024
const runtimeJournalMagic = "REMNACUST-CORE-STATE-1\n"
const runtimeUserQuotaJournalMagic = "REMNACUST-CORE-STATE-2-USER-QUOTA\n"

func runtimeJournalDirectory(path string, create bool) error {
	if !filepath.IsAbs(path) {
		return errors.New("runtime checkpoint path must be absolute")
	}
	parent := filepath.Dir(path)
	if create {
		if err := os.MkdirAll(parent, 0700); err != nil {
			return err
		}
	}
	for directory := parent; ; directory = filepath.Dir(directory) {
		info, err := os.Lstat(directory)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("untrusted runtime checkpoint directory")
		}
		if directory == parent && runtime.GOOS != "windows" && (info.Mode().Perm()&0077 != 0 || !runtimeJournalOwned(info)) {
			return errors.New("runtime checkpoint directory must be private and owned by the core")
		}
		if filepath.Dir(directory) == directory {
			break
		}
	}
	return nil
}

func LoadRuntimeJournal(path string) (*Config, error) {
	if err := runtimeJournalDirectory(path, false); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > RuntimeJournalLimit+128 {
		return nil, errors.New("invalid runtime checkpoint file")
	}
	if runtime.GOOS != "windows" && (info.Mode().Perm()&0077 != 0 || !runtimeJournalOwned(info)) {
		return nil, errors.New("runtime checkpoint file is not private")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("runtime checkpoint changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, RuntimeJournalLimit+129))
	if err != nil {
		return nil, err
	}
	magic := runtimeJournalMagic
	userQuotaFormat := bytes.HasPrefix(data, []byte(runtimeUserQuotaJournalMagic))
	if userQuotaFormat {
		magic = runtimeUserQuotaJournalMagic
	}
	if !bytes.HasPrefix(data, []byte(magic)) || len(data) < len(magic)+65 || data[len(magic)+64] != '\n' {
		return nil, errors.New("invalid runtime checkpoint header")
	}
	checksum, payload := data[len(magic):len(magic)+64], data[len(magic)+65:]
	hash := sha256.Sum256(payload)
	if len(payload) > RuntimeJournalLimit || string(checksum) != hex.EncodeToString(hash[:]) {
		return nil, errors.New("runtime checkpoint integrity check failed")
	}
	config := new(Config)
	if err := proto.Unmarshal(payload, config); err != nil {
		return nil, errors.New("runtime checkpoint cannot be decoded")
	}
	required, err := ConfigRequiresUserQuota(config)
	if err != nil || required != userQuotaFormat {
		return nil, errors.New("runtime checkpoint quota requirement does not match its format")
	}
	if _, _, err := ConfigProfileBindings(config); err != nil {
		return nil, err
	}
	return config, nil
}

// The previous checkpoint remains visible until the replacement is flushed.
func SaveRuntimeJournal(path string, config *Config) error {
	if config == nil {
		return errors.New("runtime configuration is missing")
	}
	required, requirementErr := ConfigRequiresUserQuota(config)
	if requirementErr != nil {
		return requirementErr
	}
	if _, _, err := ConfigProfileBindings(config); err != nil {
		return err
	}
	if err := runtimeJournalDirectory(path, true); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || (runtime.GOOS != "windows" && !runtimeJournalOwned(info)) {
			return errors.New("untrusted existing runtime checkpoint")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(config)
	if err != nil || len(payload) > RuntimeJournalLimit {
		return errors.New("runtime checkpoint exceeds the limit")
	}
	hash := sha256.Sum256(payload)
	magic := runtimeJournalMagic
	if required {
		magic = runtimeUserQuotaJournalMagic
	}
	data := append([]byte(magic+hex.EncodeToString(hash[:])+"\n"), payload...)
	file, err := os.CreateTemp(filepath.Dir(path), ".core-checkpoint-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		directory, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		err = directory.Sync()
		closeErr := directory.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func (s *Instance) SetRuntimeJournal(path string) { s.runtimeJournal = path }
func (s *Instance) SaveRuntimeConfiguration(config *Config) error {
	if s.profileRetired.Load() {
		return errors.New("a retired profile cannot change the current checkpoint")
	}
	if s.runtimeJournal == "" && s.profilePlan == nil && s.boundProfilePlan == nil {
		return nil
	}
	if config == nil {
		return errors.New("runtime configuration is missing")
	}
	if config != nil && s.RequiresUserQuota() {
		config = proto.Clone(config).(*Config)
		if err := RequireUserQuotaConfig(config); err != nil {
			return err
		}
	}
	if s.boundProfilePlan != nil {
		return s.boundProfilePlan.save(s.profileBindingID, config)
	}
	if s.profilePlan != nil {
		return s.profilePlan.save("", config)
	}
	if s.runtimeJournal == "" {
		return nil
	}
	return SaveRuntimeJournal(s.runtimeJournal, config)
}
