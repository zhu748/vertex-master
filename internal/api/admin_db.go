package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/db"
)

func (adm *AdminHandler) adminDatabase(w http.ResponseWriter, r *http.Request, action string) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	maintenance := db.NewMaintenance(db.CurrentDB())
	var result any
	var err error
	switch action {
	case "stats":
		result, err = maintenance.Stats(ctx)
	case "check":
		var messages []string
		messages, err = maintenance.Check(ctx)
		result = map[string]any{"ok": len(messages) == 1 && messages[0] == "ok", "results": messages}
	case "checkpoint":
		result, err = maintenance.Checkpoint(ctx)
	}
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, db.ErrNotInitialized) {
			status = http.StatusServiceUnavailable
		}
		log.Printf("[DB] 管理维护 %s 失败: %v", action, err)
		writeJSON(w, status, adminErr("数据库操作失败，请查看运行日志"))
		return
	}
	writeJSON(w, http.StatusOK, result)
}
