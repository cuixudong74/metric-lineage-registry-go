package store

import (
	"testing"
)

func TestWALAppendReplay(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := w.Append(EntryBaseUpdate, BaseUpdatePayload{
			ID:     "m",
			Points: map[string]float64{"k": float64(i)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	w2, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if w2.LastSeq() != 5 {
		t.Fatalf("last seq = %d, want 5", w2.LastSeq())
	}
	var seqs []int64
	var last BaseUpdatePayload
	if err := w2.Replay(func(e *Entry) error {
		seqs = append(seqs, e.Seq)
		if e.Type != EntryBaseUpdate {
			t.Fatalf("type = %s", e.Type)
		}
		return DecodePayload(e, &last)
	}); err != nil {
		t.Fatal(err)
	}
	if len(seqs) != 5 || seqs[0] != 1 || seqs[4] != 5 {
		t.Fatalf("seqs = %v", seqs)
	}
	if last.Points["k"] != 4 {
		t.Fatalf("last payload = %+v", last)
	}

	// Appending after reopen continues the sequence.
	e, err := w2.Append(EntryRetire, RetirePayload{ID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Seq != 6 {
		t.Fatalf("seq after reopen = %d", e.Seq)
	}
}

func TestWALTornTail(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(EntryRetire, RetirePayload{ID: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Append a partial line simulating a torn write.
	f, err := openRawAppend(dir + "/wal.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n{broken-json"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	w2, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	count := 0
	if err := w2.Replay(func(e *Entry) error {
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("replayed %d intact records, want 1", count)
	}
	if w2.LastSeq() != 1 {
		t.Fatalf("last seq = %d, want 1", w2.LastSeq())
	}
}

func TestSnapshotAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSnapshot(dir, "snap.json", []byte("{\"v\":1}")); err != nil {
		t.Fatal(err)
	}
	data, err := ReadSnapshot(dir, "snap.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{\"v\":1}" {
		t.Fatalf("snapshot data = %s", data)
	}
	missing, err := ReadSnapshot(dir, "nope.json")
	if err != nil || missing != nil {
		t.Fatalf("missing snapshot = %v %v", missing, err)
	}
}
