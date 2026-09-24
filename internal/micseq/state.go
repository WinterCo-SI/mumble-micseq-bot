package micseq

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

const stateVersion = 1

type fileState struct {
	Version  int                     `json:"version"`
	Channels map[string]*fileChannel `json:"channels"`
}

type fileMember struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	ResumeMS int64  `json:"resume_ms,omitempty"`
}

type fileDropped struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	RemainingMS int64  `json:"remaining_ms"`
	UntilUnixMS int64  `json:"until_unix_ms"`
}

type fileChannel struct {
	DurationS   int64         `json:"duration_s"`
	Target      uint32        `json:"target"`
	Speaker     *fileMember   `json:"speaker,omitempty"`
	RemainingMS int64         `json:"remaining_ms,omitempty"`
	Warned      bool          `json:"warned,omitempty"`
	Queue       []fileMember  `json:"queue"`
	Exempt      []fileMember  `json:"exempt,omitempty"`
	Finished    []fileMember  `json:"finished,omitempty"`
	Dropped     []fileDropped `json:"dropped,omitempty"`
}

// MarshalState serializes all managed channels.
func (m *Manager) MarshalState() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st := fileState{Version: stateVersion, Channels: map[string]*fileChannel{}}
	for id, cs := range m.managed {
		fc := &fileChannel{
			DurationS: int64(cs.marker.Duration / time.Second),
			Target:    cs.marker.TargetID,
			Warned:    cs.warned,
			Queue:     make([]fileMember, 0, len(cs.queue)),
			Exempt:    setMembers(cs.exempt),
			Finished:  setMembers(cs.finished),
		}
		if cs.speaker != nil {
			fc.Speaker = &fileMember{Key: cs.speaker.Key, Name: cs.speaker.Name}
			fc.RemainingMS = cs.remaining.Milliseconds()
		}
		for _, q := range cs.queue {
			fc.Queue = append(fc.Queue, fileMember{Key: q.Key, Name: q.Name, ResumeMS: q.Resume.Milliseconds()})
		}
		for key, d := range cs.dropped {
			fc.Dropped = append(fc.Dropped, fileDropped{
				Key: key, Name: d.Name,
				RemainingMS: d.Remaining.Milliseconds(),
				UntilUnixMS: d.Until.UnixMilli(),
			})
		}
		sort.Slice(fc.Dropped, func(i, j int) bool { return fc.Dropped[i].Key < fc.Dropped[j].Key })
		st.Channels[strconv.FormatUint(uint64(id), 10)] = fc
	}
	return json.MarshalIndent(st, "", "  ")
}

func setMembers(set map[string]string) []fileMember {
	out := make([]fileMember, 0, len(set))
	for k, n := range set {
		out = append(out, fileMember{Key: k, Name: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// RestoreState loads channel states saved by MarshalState. Call it before the
// first Sync; the next Sync reconciles them with who is actually present.
func (m *Manager) RestoreState(data []byte) error {
	var st fileState
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	if st.Version != stateVersion {
		return fmt.Errorf("unsupported state version %d", st.Version)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for sid, fc := range st.Channels {
		id, err := strconv.ParseUint(sid, 10, 32)
		if err != nil || fc == nil {
			continue
		}
		cs := newChannelState(uint32(id))
		cs.restored = true
		cs.marker = Marker{Duration: time.Duration(fc.DurationS) * time.Second, TargetID: fc.Target}
		for _, q := range fc.Queue {
			cs.queue = append(cs.queue, member{Key: q.Key, Name: q.Name, Resume: time.Duration(q.ResumeMS) * time.Millisecond})
		}
		for _, e := range fc.Exempt {
			cs.exempt[e.Key] = e.Name
		}
		for _, f := range fc.Finished {
			cs.finished[f.Key] = f.Name
		}
		for _, d := range fc.Dropped {
			cs.dropped[d.Key] = droppedSpeaker{
				Name:      d.Name,
				Remaining: time.Duration(d.RemainingMS) * time.Millisecond,
				Until:     time.UnixMilli(d.UntilUnixMS),
			}
		}
		if fc.Speaker != nil && fc.RemainingMS > 0 {
			cs.speaker = &member{Key: fc.Speaker.Key, Name: fc.Speaker.Name}
			cs.remaining = time.Duration(fc.RemainingMS) * time.Millisecond
			cs.warned = fc.Warned
		}
		m.managed[cs.id] = cs
	}
	return nil
}

// LoadStateFile reads a state file; a missing file yields nil data.
func LoadStateFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// SaveStateFile writes data atomically: temp file, fsync, rename.
func SaveStateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// On Windows the rename fails while another process (an editor, a virus
	// scanner) briefly holds the destination open.
	for attempt := 0; ; attempt++ {
		err = os.Rename(tmpName, path)
		if err == nil || attempt == 4 {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
}
