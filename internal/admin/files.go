package admin

import (
	"os"
	"path/filepath"
	"strings"
)

// joinUploadPath 把附件的相对路径拼到上传根目录下，并阻止目录穿越。
//
// 路径来自数据库，但依然按不可信处理：任何解析后越出根目录的路径一律返回空串，
// 调用方据此跳过删除，避免「删用户」顺带删掉服务器上的其它文件。
func joinUploadPath(root, rel string) string {
	if root == "" || rel == "" {
		return ""
	}
	if filepath.IsAbs(rel) {
		rel = strings.TrimPrefix(rel, string(filepath.Separator))
	}
	clean := filepath.Clean(rel)
	if clean == "." || strings.HasPrefix(clean, "..") {
		return ""
	}
	full := filepath.Join(root, clean)
	back, err := filepath.Rel(root, full)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return ""
	}
	return full
}

// removeFile 尽力删除附件文件；不存在视为成功（幂等）。
func removeFile(path string) error {
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}
