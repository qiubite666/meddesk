package attachments

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// fakeRepo 是 AttachmentRepo 的内存假实现（不依赖数据库）。
type fakeRepo struct {
	mu    sync.Mutex
	items map[string]*model.Attachment
}

func (f *fakeRepo) Create(_ context.Context, a *model.Attachment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *a
	f.items[a.ID] = &cp
	return nil
}

func (f *fakeRepo) Get(_ context.Context, userID, id string) (*model.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.items[id]
	if !ok || a.UserID != userID {
		return nil, store.ErrNotFound
	}
	cp := *a
	return &cp, nil
}

func (f *fakeRepo) Delete(_ context.Context, userID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.items[id]
	if !ok || a.UserID != userID {
		return store.ErrNotFound
	}
	delete(f.items, id)
	return nil
}

func (f *fakeRepo) List(_ context.Context, userID, recordID string) ([]model.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []model.Attachment{}
	for _, a := range f.items {
		if a.UserID == userID && (recordID == "" || a.RecordID == recordID) {
			out = append(out, *a)
		}
	}
	return out, nil
}

// TestSaveOpenDelete 做一次 Save → Open → Delete 往返，校验落盘路径与元数据一致。
func TestSaveOpenDelete(t *testing.T) {
	dir, err := os.MkdirTemp("", "meddesk-attachments-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	svc := NewService(&fakeRepo{items: map[string]*model.Attachment{}},
		config.UploadConfig{Dir: dir, MaxBytes: 1 << 20})

	data := []byte("0123456789abcdef-ciphertext") // 假装这是密文
	iv := []byte("0123456789ab")
	dto, err := svc.Save(ctx, "u1", "rec-1", iv, "image/png", bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if dto.ID == "" || dto.Size != int64(len(data)) || dto.Mime != "image/png" {
		t.Fatalf("DTO 元数据不符: %+v", dto)
	}

	// 落盘路径必须是 {Dir}/{userID}/{yy}/{id}.bin
	yy := time.Now().Format("06")
	want := filepath.Join(dir, "u1", yy, dto.ID+".bin")
	fi, err := os.Stat(want)
	if err != nil {
		t.Fatalf("密文未落到预期路径 %s: %v", want, err)
	}
	// Windows 下 os.Chmod 只能切换只读位，无法表达 0600，故仅类 Unix 断言权限。
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("文件权限过宽: %v", fi.Mode().Perm())
	}

	// 库中 Path 必须是相对路径，且与磁盘位置一致
	a, rc, err := svc.Open(ctx, "u1", dto.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("读回内容与写入不一致: %q", got)
	}
	if a.Path != "u1/"+yy+"/"+dto.ID+".bin" {
		t.Fatalf("Path 应为相对路径，实际 %q", a.Path)
	}

	// 越权访问必须返回 not_found
	if _, _, err := svc.Open(ctx, "u2", dto.ID); !apierr.Is(err, apierr.CodeNotFound) {
		t.Fatalf("越权访问应返回 not_found，实际 %v", err)
	}

	if err := svc.Delete(ctx, "u1", dto.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("删除后文件仍存在: %v", err)
	}
	if err := svc.Delete(ctx, "u1", dto.ID); !apierr.Is(err, apierr.CodeNotFound) {
		t.Fatalf("重复删除应返回 not_found，实际 %v", err)
	}
}

// TestSaveTooLarge 校验超出上限时返回 payload_too_large。
func TestSaveTooLarge(t *testing.T) {
	dir, err := os.MkdirTemp("", "meddesk-attachments-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	svc := NewService(&fakeRepo{items: map[string]*model.Attachment{}},
		config.UploadConfig{Dir: dir, MaxBytes: 8})
	_, err = svc.Save(context.Background(), "u1", "", make([]byte, 12), "application/octet-stream",
		bytes.NewReader(make([]byte, 32)), 32)
	if !apierr.Is(err, apierr.CodePayloadTooLarge) {
		t.Fatalf("期望 payload_too_large，实际 %v", err)
	}
}
