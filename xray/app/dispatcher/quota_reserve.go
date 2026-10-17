//go:build !wasm

package dispatcher

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"
)

const quotaReserveCapability = "remnacust-quota-reserves-v1"
const quotaLedgerLimit = 16 * 1024 * 1024
const quotaWriteAhead = int64(1024 * 1024)
const quotaLedgerPath = "/var/lib/remnanode/quota/ledger.json"

// Ceilings are cumulative, never a delta. Replaying a policy cannot refill a grant.
type quotaGrant struct {
	ID                   string `json:"id"`
	Instance             string `json:"instance"`
	Ceiling              string `json:"ceiling"`
	ExpiresAt            int64  `json:"expiresAt"`
	RegistrationRequired bool   `json:"registrationRequired,omitempty"`
}
type quotaDebit struct {
	Grant        quotaGrant
	Group, Owner string
	Cost         int64
	Fraction     int64 // fractional billed bytes, accumulated across packets
	Scale        int64 // zero preserves the original hundredths format
}
type quotaEntry struct {
	Group     string `json:"group"`
	Owner     string `json:"owner"`
	ExpiresAt int64  `json:"expiresAt"`
	Ceiling   int64  `json:"ceiling"`
	Reserved  int64  `json:"reserved"`
	Closed    bool   `json:"closed,omitempty"`
	Scale     int64  `json:"scale,omitempty"`
	Prepared  bool   `json:"prepared,omitempty"`
	Activated bool   `json:"activated,omitempty"`
	consumed  int64
	fraction  int64
}
type quotaLedger struct {
	Instance      string                `json:"instance"`
	Version       string                `json:"version"`
	Entries       map[string]quotaEntry `json:"entries"`
	RetiredBefore int64                 `json:"retiredBefore,omitempty"`
	Sequence      uint64                `json:"sequence,omitempty"`
	Checkpoint    uint64                `json:"checkpoint,omitempty"`
}
type quotaEnvelope struct {
	Payload json.RawMessage `json:"payload"`
	SHA256  string          `json:"sha256"`
}
type quotaStore struct {
	sync.Mutex
	path             string
	entries          map[string]quotaEntry
	failure          error
	retiredBefore    int64
	sequence         uint64
	fileSize         int64
	journalBytes     int64
	legacyFormat     bool
	fileInfo         os.FileInfo
	instance         string
	registeredGrants bool
	watermarks       bool
	checkpoint       uint64
}

var localQuota = &quotaStore{path: quotaLedgerPath}

func quotaFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !quotaOwned(info) || info.Size() > quotaLedgerLimit || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("quota ledger must be a bounded private regular file")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || !quotaOwned(parent) || parent.Mode()&os.ModeSymlink != 0 || (runtime.GOOS != "windows" && parent.Mode().Perm()&0077 != 0) {
		return nil, errors.New("quota ledger directory must be private")
	}
	for dir := filepath.Dir(filepath.Dir(path)); ; dir = filepath.Dir(dir) {
		entry, err := os.Lstat(dir)
		if err != nil || !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("untrusted quota ledger ancestor")
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("quota ledger changed while opening")
	}
	return io.ReadAll(io.LimitReader(f, quotaLedgerLimit+1))
}

func (s *quotaStore) load() error {
	if s.failure != nil {
		return s.failure
	}
	if s.entries != nil {
		return nil
	}
	data, err := quotaFile(s.path)
	if err != nil {
		return err
	}
	frames := bytes.Split(data, []byte{'\n'})
	if len(frames) > 1 && len(frames[len(frames)-1]) != 0 {
		return errors.New("incomplete quota journal record")
	}
	entries := make(map[string]quotaEntry)
	var instance string
	var sequence uint64
	var retired int64
	var journalBytes int64
	for index, frame := range frames {
		if len(frame) == 0 {
			if index == len(frames)-1 && index > 0 {
				break
			}
			return errors.New("empty quota journal record")
		}
		var envelope quotaEnvelope
		if json.Unmarshal(frame, &envelope) != nil {
			return errors.New("invalid quota ledger envelope")
		}
		sum := sha256.Sum256(envelope.Payload)
		if envelope.SHA256 != hex.EncodeToString(sum[:]) {
			return errors.New("quota ledger checksum mismatch")
		}
		var record quotaLedger
		if json.Unmarshal(envelope.Payload, &record) != nil || quotaJournalVersion(record.Version) == 0 || (s.registeredGrants && quotaJournalVersion(record.Version) < 2) || (s.watermarks && quotaJournalVersion(record.Version) < 3) || record.Checkpoint < s.checkpoint || record.Checkpoint > record.Sequence || (record.Checkpoint > 0 && quotaJournalVersion(record.Version) < 3) || !validQuotaID(record.Instance) || (index > 0 && record.Instance != instance) || record.Entries == nil || record.RetiredBefore < retired || (index > 0 && record.Sequence != sequence+1) {
			return errors.New("invalid quota journal sequence")
		}
		s.registeredGrants = quotaJournalVersion(record.Version) >= 2
		s.watermarks = quotaJournalVersion(record.Version) >= 3
		s.checkpoint = record.Checkpoint
		for id, e := range record.Entries {
			if !validQuotaScale(e.Scale) || !validQuotaID(id) || e.Group == "" || e.Owner == "" || e.ExpiresAt <= 0 || e.Ceiling < 0 || e.Reserved < 0 || e.Reserved > e.Ceiling || ((e.Prepared || e.Activated) && !s.registeredGrants) || (e.Activated && !e.Prepared) || (e.Prepared && !e.Activated && e.Reserved != 0) {
				return errors.New("invalid quota ledger entry")
			}
			if previous, exists := entries[id]; exists && (previous.Group != e.Group || previous.Owner != e.Owner || previous.ExpiresAt != e.ExpiresAt || previous.Prepared != e.Prepared || (previous.Activated && !e.Activated) || e.Ceiling < previous.Ceiling || (previous.Closed && !e.Closed) || (e.Reserved < previous.Reserved && !(e.Closed && !previous.Closed)) || quotaScale(previous.Scale) != quotaScale(e.Scale)) {
				return errors.New("non-monotonic quota journal")
			}
			e.consumed = e.Reserved
			entries[id] = e
		}
		if len(entries) > 100000 {
			return errors.New("quota ledger capacity exceeded")
		}
		instance = record.Instance
		sequence = record.Sequence
		retired = record.RetiredBefore
		if index > 0 {
			journalBytes += int64(len(frame) + 1)
		}
	}
	if len(entries) == 0 && len(bytes.TrimSpace(data)) == 0 {
		return errors.New("missing quota checkpoint")
	}
	info, err := os.Lstat(s.path)
	if err != nil {
		return err
	}
	s.instance = instance
	s.entries = entries
	s.sequence = sequence
	s.retiredBefore = retired
	s.fileSize = int64(len(data))
	s.journalBytes = journalBytes
	s.legacyFormat = len(frames) == 1
	s.fileInfo = info
	return nil
}

func validQuotaID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for index, ch := range id {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if ch != '-' {
				return false
			}
			continue
		}
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

func quotaRecord(entries map[string]quotaEntry, retired int64, sequence uint64, instance string) ([]byte, error) {
	return quotaVersionedRecord(entries, retired, sequence, instance, false)
}

func quotaVersionedRecord(entries map[string]quotaEntry, retired int64, sequence uint64, instance string, registered bool) ([]byte, error) {
	return quotaCheckpointRecord(entries, retired, sequence, instance, registered, false, 0)
}

func quotaCheckpointRecord(entries map[string]quotaEntry, retired int64, sequence uint64, instance string, registered, watermarks bool, checkpoint uint64) ([]byte, error) {
	version := quotaReserveCapability
	if registered {
		version = quotaRegistrationCapability
	}
	if watermarks {
		version = quotaWatermarkCapability
	}
	payload, err := json.Marshal(quotaLedger{Instance: instance, Version: version, Entries: entries, RetiredBefore: retired, Sequence: sequence, Checkpoint: checkpoint})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload)
	data, err := json.Marshal(quotaEnvelope{payload, hex.EncodeToString(sum[:])})
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Most flushes append only the changed grants. Periodic atomic compaction bounds
// startup time and disk usage without rewriting every subscriber on each packet.
func (s *quotaStore) save(updates map[string]quotaEntry, now time.Time) error {
	if s.sequence == ^uint64(0) {
		return errors.New("quota sequence exhausted")
	}
	nextSequence := s.sequence + 1
	data, err := quotaCheckpointRecord(updates, s.retiredBefore, nextSequence, s.instance, s.registeredGrants, s.watermarks, s.checkpoint)
	if err != nil {
		return err
	}
	info, err := os.Lstat(s.path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !quotaOwned(info) || !os.SameFile(info, s.fileInfo) || info.Size() != s.fileSize || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return errors.New("quota ledger changed outside the core")
	}
	newCount := len(s.entries)
	for key := range updates {
		if _, exists := s.entries[key]; !exists {
			newCount++
		}
	}
	compact := newCount > 100000 || s.legacyFormat || s.journalBytes+int64(len(data)) >= 4*1024*1024 || s.fileSize+int64(len(data)) > quotaLedgerLimit
	if !compact {
		f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		opened, err := f.Stat()
		if err != nil || !os.SameFile(info, opened) || opened.Size() != s.fileSize {
			f.Close()
			return errors.New("quota ledger changed while opening")
		}
		n, err := f.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		s.fileSize += int64(len(data))
		s.journalBytes += int64(len(data))
		s.sequence = nextSequence
		return nil
	}
	next := make(map[string]quotaEntry, len(s.entries)+len(updates))
	retired := s.retiredBefore
	for key, e := range s.entries {
		if e.ExpiresAt <= now.UnixMilli() {
			retired = max(retired, e.ExpiresAt)
			continue
		}
		next[key] = e
	}
	for key, e := range updates {
		next[key] = e
	}
	if len(next) > 100000 {
		return errors.New("quota ledger capacity exceeded")
	}
	data, err = quotaCheckpointRecord(next, retired, nextSequence, s.instance, s.registeredGrants, s.watermarks, s.checkpoint)
	if err != nil {
		return err
	}
	if len(data) > quotaLedgerLimit {
		return errors.New("quota ledger capacity exceeded")
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".quota-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		d, err := os.Open(filepath.Dir(s.path))
		if err != nil {
			return err
		}
		err = d.Sync()
		closeErr = d.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	info, err = os.Lstat(s.path)
	if err != nil {
		return err
	}
	s.entries = next
	s.retiredBefore = retired
	s.sequence = nextSequence
	s.fileInfo = info
	s.fileSize = int64(len(data))
	s.journalBytes = 0
	s.legacyFormat = false
	return nil
}

// Called after speed waits and access revalidation. All overlapping quotas are
// checked together, then flushed before any protected bytes reach a writer.
func (s *quotaStore) debit(debits []quotaDebit, now time.Time) error {
	if len(debits) == 0 {
		return nil
	}
	s.Lock()
	defer s.Unlock()
	if err := s.load(); err != nil {
		return fmt.Errorf("quota state unavailable: %w", err)
	}
	updates := make(map[string]quotaEntry, len(debits))
	flush := false
	for _, d := range debits {
		scale := quotaScale(d.Scale)
		cap, err := strconv.ParseInt(d.Grant.Ceiling, 10, 64)
		if !validQuotaScale(d.Scale) || err != nil || d.Grant.Instance != s.instance || cap < 0 || !validQuotaID(d.Grant.ID) || d.Cost < 0 || d.Fraction < 0 || d.Fraction >= scale || d.Grant.ExpiresAt <= now.UnixMilli() || d.Grant.ExpiresAt <= s.retiredBefore {
			return errors.New("invalid or expired quota reserve")
		}
		entry, exists := s.entries[d.Grant.ID]
		if (d.Grant.RegistrationRequired && (!s.registeredGrants || !exists || !entry.Prepared || !entry.Activated)) || (entry.Prepared && !d.Grant.RegistrationRequired) {
			return errors.New("quota reserve registration is not activated")
		}
		if exists && entry.Closed {
			return errors.New("quota reserve was permanently closed")
		}
		if _, duplicate := updates[d.Grant.ID]; duplicate {
			return errors.New("duplicate quota reserve binding")
		}
		if !exists {
			entry = quotaEntry{Group: d.Group, Owner: d.Owner, ExpiresAt: d.Grant.ExpiresAt, Ceiling: cap, Scale: storedQuotaScale(scale)}
			flush = true
		} else if entry.Group != d.Group || entry.Owner != d.Owner || entry.ExpiresAt != d.Grant.ExpiresAt || cap < entry.Ceiling || quotaScale(entry.Scale) != scale {
			return errors.New("quota reserve replay or binding mismatch")
		}
		if cap > entry.Ceiling {
			entry.Ceiling = cap
			flush = true
		}
		cost, fraction := d.Cost, entry.fraction+d.Fraction
		if fraction >= scale {
			if cost == 1<<63-1 {
				return errors.New("quota charge overflow")
			}
			cost++
			fraction -= scale
		}
		// consumed is ceil(total units / scale). Retain the fractional part
		// in RAM, so tiny packets cannot each cost an entire extra billed byte.
		if fraction > 0 && entry.fraction == 0 {
			if cost == 1<<63-1 {
				return errors.New("quota charge overflow")
			}
			cost++
		} else if fraction == 0 && entry.fraction > 0 {
			cost--
		}
		if cost < 0 || cost > cap-entry.consumed {
			return errors.New("local quota reserve exhausted")
		}
		entry.consumed += cost
		entry.fraction = fraction
		if entry.consumed > entry.Reserved {
			entry.Reserved = entry.consumed + min(quotaWriteAhead, cap-entry.consumed)
			flush = true
		}
		updates[d.Grant.ID] = entry
	}
	if flush {
		if err := s.save(updates, now); err != nil {
			s.failure = err
			return errors.New("quota checkpoint failed; protected traffic stopped")
		}
	}
	for k, v := range updates {
		s.entries[k] = v
	}
	return nil
}

// A missing grant in an authoritative policy is an irreversible revocation.
// Its exact in-process charge is flushed before acknowledgement; after a crash,
// load() has conservatively charged the entire prewritten block instead.
func (s *quotaStore) reconcile(policy hostPolicyDocument, now time.Time) error {
	s.Lock()
	defer s.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	checkpoint, checkpointErr := s.validateCheckpoint(policy)
	if checkpointErr != nil {
		return checkpointErr
	}
	active := make(map[string]bool)
	registrations, err := s.planRegistrations(policy, now)
	if err != nil {
		return err
	}
	for key, group := range policy.Groups {
		if !validQuotaScale(group.MultiplierScale) {
			return errors.New("invalid quota multiplier scale")
		}
		for owner, grant := range group.Reserves {
			if _, collision := registrations[grant.ID]; collision {
				return errors.New("quota registration overlaps active grant")
			}
			if !group.ReserveRequired || group.UnlimitedOwners[owner] || grant.Instance != s.instance || !validQuotaID(grant.ID) || active[grant.ID] {
				return errors.New("invalid quota reserve binding")
			}
			ceiling, err := strconv.ParseInt(grant.Ceiling, 10, 64)
			if err != nil || ceiling < 0 || grant.ExpiresAt <= s.retiredBefore {
				return errors.New("invalid quota ceiling")
			}
			active[grant.ID] = true
			previous, exists := s.entries[grant.ID]
			if (grant.RegistrationRequired && (policy.QuotaRegistrations != quotaRegistrationCapability || !exists || !previous.Prepared)) || (previous.Prepared && !grant.RegistrationRequired) {
				return errors.New("quota activation requires a prior durable registration")
			}
			if exists && (previous.Closed || previous.Owner != owner || previous.Group != key || ceiling < previous.Ceiling || grant.ExpiresAt != previous.ExpiresAt || quotaScale(previous.Scale) != quotaScale(group.MultiplierScale)) {
				return errors.New("closed or mismatched quota reserve cannot be replayed")
			}
			if !exists {
				if grant.ExpiresAt <= now.UnixMilli() {
					return errors.New("cannot register an expired quota reserve")
				}
				registrations[grant.ID] = quotaEntry{Group: key, Owner: owner, ExpiresAt: grant.ExpiresAt, Ceiling: ceiling, Scale: storedQuotaScale(quotaScale(group.MultiplierScale))}
			} else if ceiling > previous.Ceiling || (previous.Prepared && !previous.Activated) {
				previous.Ceiling = ceiling
				previous.Activated = previous.Prepared
				registrations[grant.ID] = previous
			}
		}
	}
	if (policy.QuotaRegistrations == quotaRegistrationCapability && !s.registeredGrants) || (policy.QuotaCheckpoint != nil && (!s.watermarks || checkpoint > s.checkpoint)) {
		s.registeredGrants = true
		s.watermarks = s.watermarks || policy.QuotaCheckpoint != nil
		s.checkpoint = checkpoint
		if err := s.save(map[string]quotaEntry{}, now); err != nil {
			s.failure = err
			return err
		}
	}
	updates := make(map[string]quotaEntry)
	flush := func() error {
		if len(updates) == 0 {
			return nil
		}
		if err := s.save(updates, now); err != nil {
			s.failure = err
			return err
		}
		for id, entry := range updates {
			s.entries[id] = entry
		}
		clear(updates)
		return nil
	}
	// A grant must exist durably before the policy ACK, even if no client has
	// used it. Removing that grant later can then prove a closed zero charge.
	for id, entry := range registrations {
		updates[id] = entry
		if len(updates) >= 1000 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	ids := make([]string, 0, len(s.entries))
	for id := range s.entries {
		ids = append(ids, id)
	}
	for _, id := range ids {
		entry, exists := s.entries[id]
		if !exists || entry.Closed || active[id] || (entry.Prepared && !entry.Activated) {
			continue
		}
		entry.Closed = true
		entry.Reserved = entry.consumed
		updates[id] = entry
		if len(updates) >= 1000 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}
