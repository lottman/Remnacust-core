//go:build !wasm

package dispatcher

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

	"github.com/google/uuid"
)

const quotaRecoveryFenceCapability = "remnacust-quota-recovery-fence-v1"
const quotaRecoveryFencePath = "/var/lib/remnanode/quota/recovery.json"
const quotaRecoveryFenceStatusPath = "/var/lib/remnanode/quota/recovery.status.json"

type quotaRecoveryFence struct {
	Version    string  `json:"version"`
	Generation string  `json:"generation"`
	Token      string  `json:"token"`
	State      string  `json:"state"`
	Sequence   string  `json:"sequence"`
	Previous   *string `json:"previous"`
	Digest     string  `json:"digest"`
}

func recoveryFenceDigest(value quotaRecoveryFence) string {
	previous := "-"
	if value.Previous != nil {
		previous = *value.Previous
	}
	data := value.Version + "\n" + value.Generation + "\n" + value.Token + "\n" + value.State + "\n" + value.Sequence + "\n" + previous + "\n"
	digest := sha256.Sum256([]byte(data))
	return hex.EncodeToString(digest[:])
}

func validRecoveryFence(value quotaRecoveryFence) bool {
	canonicalUUID := func(text string) bool {
		parsed, err := uuid.Parse(text)
		return err == nil && parsed.String() == text && parsed != uuid.Nil
	}
	sequence, err := strconv.ParseUint(value.Sequence, 10, 63)
	if err != nil || sequence == 0 || strconv.FormatUint(sequence, 10) != value.Sequence ||
		value.Version != quotaRecoveryFenceCapability || !canonicalUUID(value.Generation) || !canonicalUUID(value.Token) ||
		(value.State != "FENCED" && value.State != "RELEASED") {
		return false
	}
	if value.Previous != nil {
		decoded, err := hex.DecodeString(*value.Previous)
		if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != *value.Previous {
			return false
		}
	}
	if (sequence == 1) != (value.Previous == nil) || (value.State == "RELEASED" && sequence == 1) {
		return false
	}
	return recoveryFenceDigest(value) == value.Digest
}

func readRecoveryRecord(path string) (*quotaRecoveryFence, error) {
	before, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > 8192 {
		return nil, errors.New("invalid private recovery fence")
	}
	data, err := quotaFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > 8192 {
		return nil, errors.New("invalid recovery fence size")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var value quotaRecoveryFence
	if decoder.Decode(&value) != nil || !validRecoveryFence(value) {
		return nil, errors.New("invalid recovery fence identity")
	}
	var tail any
	if decoder.Decode(&tail) != io.EOF {
		return nil, errors.New("trailing recovery fence data")
	}
	return &value, nil
}

func readRecoveryFence(path string) (*quotaRecoveryFence, bool, error) {
	required, err := readRecoveryRecord(filepath.Join(filepath.Dir(path), "recovery-required.json"))
	if err != nil {
		return nil, false, err
	}
	if required != nil && (required.State != "FENCED" || required.Sequence != "1") {
		return nil, false, errors.New("invalid immutable recovery requirement")
	}
	value, err := readRecoveryRecord(path)
	if required != nil && (err != nil || value == nil || (value.Sequence == "1" && value.Digest != required.Digest)) {
		return required, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if required == nil && value != nil {
		return nil, false, errors.New("missing immutable recovery requirement")
	}
	return value, false, nil
}

// This gate is independent of the quota policy. An ordinary policy update or
// a missing file cannot release traffic while a restored database is pending.
func publishRecoveryFence(next *quotaRecoveryFence, initial bool, fallback ...bool) error {
	hostPolicyUpdates.Lock()
	defer hostPolicyUpdates.Unlock()
	hostPolicies.Lock()
	defer hostPolicies.Unlock()
	previous := hostPolicies.recoveryFence
	invalid := func() error {
		hostPolicies.recoveryInvalid = true
		notifyHostPolicyLocked()
		return errors.New("recovery fence requires explicit reconciliation")
	}
	if next != nil && !validRecoveryFence(*next) {
		return invalid()
	}
	if !initial && !hostPolicies.recoveryInvalid && ((next == nil && previous == nil) || (next != nil && previous != nil && next.Digest == previous.Digest)) {
		return nil
	}
	// Replacing lost control metadata by the immutable first FENCED record is
	// restrictive only. It never releases a connection or changes quota credit.
	restrictiveFallback := len(fallback) == 1 && fallback[0] && next != nil && next.State == "FENCED" && next.Sequence == "1"
	if !initial && !restrictiveFallback {
		if previous != nil && (next == nil || (next.Digest != previous.Digest &&
			(next.Previous == nil || *next.Previous != previous.Digest ||
				next.Sequence != nextRecoverySequence(previous.Sequence) ||
				(next.State == "RELEASED" && (previous.State != "FENCED" || next.Generation != previous.Generation))))) {
			return invalid()
		}
		if previous == nil && next != nil && (next.State != "FENCED" || next.Previous != nil || next.Sequence != "1") {
			return invalid()
		}
	}
	hostPolicies.recoveryFence = next
	hostPolicies.recoveryInvalid = false
	if next == nil || next.State == "RELEASED" {
		notifyHostPolicyLocked()
		return nil
	}
	// Admissions already flushing their journal finish their counters before
	// ACK. New managed identities stop at the gate, retaining their connections.
	keys := make([]string, 0, len(hostPolicies.admissions))
	hostPolicies.pendingDrain = 0
	for key, count := range hostPolicies.admissions {
		hostPolicies.frozen[key] = true
		keys = append(keys, key)
		hostPolicies.pendingDrain += count
	}
	for hostPolicies.pendingDrain > 0 {
		wake := hostPolicies.changed
		hostPolicies.Unlock()
		<-wake
		hostPolicies.Lock()
	}
	for _, key := range keys {
		delete(hostPolicies.frozen, key)
	}
	notifyHostPolicyLocked()
	return nil
}

func nextRecoverySequence(previous string) string {
	sequence, err := strconv.ParseUint(previous, 10, 63)
	if err != nil || sequence == (1<<63)-1 {
		return ""
	}
	return strconv.FormatUint(sequence+1, 10)
}

func writeRecoveryFenceStatus(path string, value *quotaRecoveryFence) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !quotaOwned(info) || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return errors.New("recovery directory must remain private")
	}
	state := "NONE"
	var digest *string
	if value != nil {
		state, digest = value.State, &value.Digest
	}
	data, err := json.Marshal(map[string]any{"version": quotaRecoveryFenceCapability,
		"pid": os.Getpid(), "coreIncarnation": coreIncarnationCapability, "coreProcessId": coreIncarnation, "state": state, "digest": digest})
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Size() == int64(len(data)) && info.Mode().Perm()&0077 == 0 {
		if saved, err := os.ReadFile(path); err == nil && bytes.Equal(saved, data) {
			return nil
		}
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".recovery-status-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), path)
	}
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func refreshRecoveryFence(path, status string, initial bool) error {
	value, fallback, err := readRecoveryFence(path)
	if err != nil {
		hostPolicies.Lock()
		hostPolicies.recoveryInvalid = true
		notifyHostPolicyLocked()
		hostPolicies.Unlock()
		return err
	}
	if err := publishRecoveryFence(value, initial, fallback); err != nil {
		return err
	}
	return writeRecoveryFenceStatus(status, value)
}
