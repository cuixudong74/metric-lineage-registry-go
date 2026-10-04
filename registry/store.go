package registry

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// eventKind 标识持久化事件类型。
type eventKind int

const (
	evRegister eventKind = iota + 1
	evRetire
	evData
)

func (k eventKind) string() string {
	switch k {
	case evRegister:
		return "register"
	case evRetire:
		return "retire"
	case evData:
		return "data"
	default:
		return "unknown"
	}
}

// event 是一条"已提交、可重放"的持久化记录。
// Seq 在单个存储内全局单调递增，既是提交顺序也是去重依据：
// 恢复时逐条重放，崩溃产生的半条记录被忽略；未持锁的内存状态从未写入，
// 因此恢复出的定义、版本、血缘一定对应某个完整提交。
type event struct {
	Seq     int64             `json:"seq"`
	Kind    string            `json:"kind"`
	ID      string            `json:"id,omitempty"`
	Version int               `json:"version,omitempty"`
	Def     *MetricDefinition `json:"def,omitempty"`
	DataVer int64             `json:"data_ver,omitempty"`
	Dims    map[string]string `json:"dims,omitempty"`
	Value   float64           `json:"value,omitempty"`
}

func kindOf(s string) (eventKind, error) {
	switch s {
	case "register":
		return evRegister, nil
	case "retire":
		return evRetire, nil
	case "data":
		return evData, nil
	default:
		return 0, fmt.Errorf("%w: unknown event kind %q", ErrStore, s)
	}
}

// store 是已提交状态的持久化接口。
// 约定：Append 成功返回即表示事件已落盘（sync=true 时已 fsync）。
type store interface {
	Append(ev *event) error
	Replay(fn func(ev *event) error) error
	LastSeq() int64
	Close() error
}

// ---------------- 内存存储 ----------------

type memoryStore struct {
	mu  sync.Mutex
	evs []*event
}

func newMemoryStore() store {
	return &memoryStore{}
}

func (m *memoryStore) Append(ev *event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ev.Seq != int64(len(m.evs))+1 {
		return fmt.Errorf("%w: non-contiguous event seq %d, want %d",
			ErrStore, ev.Seq, len(m.evs)+1)
	}
	cp := *ev
	m.evs = append(m.evs, &cp)
	return nil
}

func (m *memoryStore) Replay(fn func(ev *event) error) error {
	m.mu.Lock()
	evs := make([]*event, len(m.evs))
	copy(evs, m.evs)
	m.mu.Unlock()
	for _, ev := range evs {
		if err := fn(ev); err != nil {
			return err
		}
	}
	return nil
}

func (m *memoryStore) LastSeq() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.evs))
}

func (m *memoryStore) Close() error { return nil }

// ---------------- JSON Lines 文件存储 ----------------

// fileStore 以追加写的 JSON Lines 保存事件。
// 每行一个完整 JSON；崩溃时最后一行可能截断，恢复时跳过无法解析的半行。
type fileStore struct {
	mu      sync.Mutex
	f       *os.File
	path    string
	sync    bool
	lastSeq int64
}

func newFileStore(path string, sync bool) (store, error) {
	if path == "" {
		return newMemoryStore(), nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("%w: open %s: %v", ErrStore, path, err)
	}
	fs := &fileStore{f: f, path: path, sync: sync}
	if err := fs.recoverSeq(); err != nil {
		f.Close()
		return nil, err
	}
	return fs, nil
}

// recoverSeq 扫描已有日志，校验序号连续并记录最后一个完整事件的序号。
func (fs *fileStore) recoverSeq() error {
	zero, err := os.Open(fs.path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
	defer zero.Close()
	sc := bufio.NewScanner(zero)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var last int64
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			// 崩溃残留的半条/坏行：停在此处，后续内容视为未提交。
			break
		}
		if ev.Seq != last+1 {
			return fmt.Errorf("%w: event log gap at seq %d (last %d)", ErrStore, ev.Seq, last)
		}
		last = ev.Seq
	}
	fs.lastSeq = last
	return nil
}

func (fs *fileStore) Append(ev *event) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if ev.Seq != fs.lastSeq+1 {
		return fmt.Errorf("%w: non-contiguous event seq %d, want %d",
			ErrStore, ev.Seq, fs.lastSeq+1)
	}
	buf, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("%w: marshal: %v", ErrStore, err)
	}
	buf = append(buf, '\n')
	if _, err := fs.f.Write(buf); err != nil {
		return fmt.Errorf("%w: write: %v", ErrStore, err)
	}
	if fs.sync {
		if err := fs.f.Sync(); err != nil {
			return fmt.Errorf("%w: fsync: %v", ErrStore, err)
		}
	}
	fs.lastSeq = ev.Seq
	return nil
}

func (fs *fileStore) Replay(fn func(ev *event) error) error {
	f, err := os.Open(fs.path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			// 半行：日志到此为止都是完整提交。
			break
		}
		if err := fn(&ev); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (fs *fileStore) LastSeq() int64 {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.lastSeq
}

func (fs *fileStore) Close() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.f == nil {
		return nil
	}
	if fs.sync {
		_ = fs.f.Sync()
	}
	err := fs.f.Close()
	fs.f = nil
	return err
}

// openAppend 以只追加方式打开日志（测试构造崩溃尾部使用）。
func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
}
