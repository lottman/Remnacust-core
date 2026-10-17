package command

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/xtls/xray-core/common/quotausage"
	"github.com/xtls/xray-core/common/uuid"
)

const usageJournalPath = "/var/lib/remnanode/usage/receipt.json"
const usageJournalLimit = 8 * 1024 * 1024
const usageJournalMagic = "REMNACUST-USAGE-1\n"

type usageValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type usageFrame struct {
	Sequence      string       `json:"sequence"`
	CoreProcessID string       `json:"coreProcessId"`
	CapturedAt    string       `json:"capturedAt"`
	Stats         []usageValue `json:"stats"`
}

type usageJournal struct {
	Version   int         `json:"version"`
	Stream    string      `json:"stream"`
	Acked     string      `json:"acked"`
	AckDigest string      `json:"ackDigest"`
	Pending   *usageFrame `json:"pending"`
}

func usageUUID(text string) bool {
	id, err := uuid.ParseString(text)
	return err == nil && len(text) == 36 && id.String() == text && id[6]>>4 >= 1 && id[6]>>4 <= 8 && id[8]>>6 == 2
}

func usageNumber(text string, zero bool) (int64, bool) {
	n, err := strconv.ParseInt(text, 10, 64)
	return n, err == nil && n >= 0 && (zero || n > 0) && strconv.FormatInt(n, 10) == text
}

func usageDigest(value any) string {
	data, _ := json.Marshal(value)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func usageHash(text string) bool {
	decoded, err := hex.DecodeString(text)
	return err == nil && len(decoded) == 32 && strings.ToLower(text) == text
}

func usageDirectory(path string, create bool) error {
	if !filepath.IsAbs(path) {
		return errors.New("usage journal path must be absolute")
	}
	parent := filepath.Dir(path)
	if create {
		// Validate existing ancestors before MkdirAll, which follows links.
		// A rejected path must not create a directory outside its state tree.
		for dir := parent; ; dir = filepath.Dir(dir) {
			info, err := os.Lstat(dir)
			if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
				return errors.New("untrusted usage journal ancestor")
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
		if err := os.MkdirAll(parent, 0700); err != nil {
			return err
		}
	}
	for dir := parent; ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (dir == parent && !usagePrivate(info)) {
			return errors.New("untrusted usage journal directory")
		}
		if filepath.Dir(dir) == dir {
			return nil
		}
	}
}

func usagePrivate(info os.FileInfo) bool {
	return runtime.GOOS == "windows" || (usageOwned(info) && info.Mode().Perm()&0077 == 0)
}

func readUsageJournal(path string) (*usageJournal, error) {
	if err := usageDirectory(path, false); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !usagePrivate(info) || info.Size() > usageJournalLimit+128 {
		return nil, errors.New("invalid usage journal file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("usage journal changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, usageJournalLimit+129))
	if err != nil {
		return nil, err
	}
	if len(data) < len(usageJournalMagic)+65 || !bytes.HasPrefix(data, []byte(usageJournalMagic)) || data[len(usageJournalMagic)+64] != '\n' {
		return nil, errors.New("invalid usage journal header")
	}
	checksum, payload := data[len(usageJournalMagic):len(usageJournalMagic)+64], data[len(usageJournalMagic)+65:]
	hash := sha256.Sum256(payload)
	if len(payload) > usageJournalLimit || string(checksum) != hex.EncodeToString(hash[:]) {
		return nil, errors.New("usage journal integrity check failed")
	}
	var state usageJournal
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, errors.New("invalid usage journal document")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing usage journal document")
	}
	acked, valid := usageNumber(state.Acked, true)
	if state.Version != 1 || !usageUUID(state.Stream) || !valid || (acked == 0 && state.AckDigest != "") || (acked > 0 && !usageHash(state.AckDigest)) {
		return nil, errors.New("invalid usage journal sequence")
	}
	if frame := state.Pending; frame != nil {
		sequence, ok := usageNumber(frame.Sequence, false)
		_, timestamp := usageNumber(frame.CapturedAt, false)
		if !ok || sequence <= acked || sequence-acked != 1 || !timestamp || !usageUUID(frame.CoreProcessID) || len(frame.Stats) > 100000 {
			return nil, errors.New("invalid pending usage frame")
		}
		previous := ""
		for _, stat := range frame.Stats {
			_, valid := usageNumber(stat.Value, false)
			if !valid || !quotausage.IsUsageCounter(stat.Name) || len(stat.Name) > 1024 || stat.Name <= previous || (!strings.HasSuffix(stat.Name, ">>>traffic>>>uplink") && !strings.HasSuffix(stat.Name, ">>>traffic>>>downlink")) {
				return nil, errors.New("invalid usage counter")
			}
			previous = stat.Name
		}
	}
	return &state, nil
}

func saveUsageJournal(path string, state *usageJournal) error {
	if err := usageDirectory(path, true); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || !usagePrivate(info) {
			return errors.New("untrusted existing usage journal")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	payload, err := json.Marshal(state)
	if err != nil || len(payload) > usageJournalLimit {
		return errors.New("usage frame exceeds journal limit")
	}
	hash := sha256.Sum256(payload)
	data := append([]byte(usageJournalMagic+hex.EncodeToString(hash[:])+"\n"), payload...)
	file, err := os.CreateTemp(filepath.Dir(path), ".usage-*")
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
		return closeErr
	}
	return nil
}
