package api

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/store"
)

const maxUpload = 16 << 20

// Upload 收凭证与回执文件，落本地磁盘，返回 file_ref。
// 放款依据是银行凭证，所以这条路径必须真的存下东西，不能只发个假 ref。
func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		httpx.Error(w, httpx.BadRequest("expected a multipart form with a 'file' field"))
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		httpx.Error(w, httpx.BadRequest("missing 'file' field"))
		return
	}
	defer f.Close()

	if err := os.MkdirAll(h.Cfg.UploadDir, 0o755); err != nil {
		httpx.Error(w, err)
		return
	}
	id := store.NewID()
	ext := filepath.Ext(hdr.Filename)
	// 只保留扩展名，不保留用户给的路径——上传的文件名不该决定落盘位置。
	ref := id + strings.ToLower(ext)
	dst, err := os.Create(filepath.Join(h.Cfg.UploadDir, ref))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer dst.Close()
	n, err := io.Copy(dst, io.LimitReader(f, maxUpload))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"file_ref": ref, "filename": hdr.Filename, "size_bytes": n,
		"url": fmt.Sprintf("/api/v1/uploads/%s", ref),
	})
}

func (h *Handler) ServeUpload(w http.ResponseWriter, r *http.Request) {
	ref := filepath.Base(r.URL.Path)
	http.ServeFile(w, r, filepath.Join(h.Cfg.UploadDir, ref))
}
