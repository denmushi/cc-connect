package codex

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type nativeProject struct {
	ID    string `json:"id"`
	Roots []struct {
		Path string `json:"path"`
	} `json:"roots"`
}

func canonicalProjectPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return filepath.Clean(abs)
}

// 优先精确匹配，其次选择最长的父目录；避免 src 被更宽泛的 home 项目吞掉。
func matchingProject(projects []nativeProject, cwd string) string {
	cwd = canonicalProjectPath(cwd)
	bestID, bestLen := "", -1
	for _, project := range projects {
		for _, root := range project.Roots {
			if strings.TrimSpace(root.Path) == "" {
				continue
			}
			path := canonicalProjectPath(root.Path)
			rel, err := filepath.Rel(path, cwd)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			if len(path) > bestLen {
				bestID, bestLen = project.ID, len(path)
			}
		}
	}
	return bestID
}

func (s *appServerSession) desktopProject() (string, error) {
	var projects []nativeProject
	cursor := ""
	seen := map[string]bool{}
	for pages := 0; ; pages++ {
		if pages >= 100 {
			return "", fmt.Errorf("project/list exceeded pagination limit")
		}
		params := map[string]any{"limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data       []nativeProject `json:"data"`
			NextCursor *string         `json:"nextCursor"`
		}
		if err := s.requestWithTimeout("project/list", params, &page, 5*time.Second); err != nil {
			return "", err
		}
		projects = append(projects, page.Data...)
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		cursor = *page.NextCursor
		if seen[cursor] {
			return "", fmt.Errorf("project/list returned a repeated cursor")
		}
		seen[cursor] = true
	}
	if id := matchingProject(projects, s.GetWorkDir()); id != "" {
		return id, nil
	}
	cwd := canonicalProjectPath(s.GetWorkDir())
	var result struct {
		Project nativeProject `json:"project"`
	}
	// 原生幂等键保证多个群同时首次使用一个目录时不会创建重复项目。
	err := s.requestWithTimeout("project/create", map[string]any{
		"idempotencyKey": fmt.Sprintf("cc-connect:%x", sha256.Sum256([]byte(cwd))),
		"name":           filepath.Base(cwd), "roots": []map[string]string{{"path": cwd}},
	}, &result, 5*time.Second)
	if err == nil && result.Project.ID == "" {
		err = fmt.Errorf("project/create returned no project ID")
	}
	return result.Project.ID, err
}

func (s *appServerSession) nativeProjectForStart() string {
	if core.NativeSessionName(s.ctx) == "" {
		return ""
	}
	id, err := s.desktopProject()
	if err != nil {
		// 旧版 Codex 没有项目接口时保留聊天能力，不冒充关联成功。
		slog.Warn("codex native project registration unavailable", "error", err)
		s.nativeMetadataErr = errors.Join(s.nativeMetadataErr, err)
	}
	return id
}

func (s *appServerSession) syncNativeMetadata(name, projectID *string) {
	title := core.NativeSessionName(s.ctx)
	if title == "" {
		return
	}
	if projectID == nil || *projectID == "" {
		if id := s.nativeProjectForStart(); id != "" {
			if err := s.requestWithTimeout("thread/metadata/update", map[string]any{"threadId": s.CurrentSessionID(), "projectId": id}, nil, 5*time.Second); err != nil {
				s.nativeMetadataErr = errors.Join(s.nativeMetadataErr, err)
				slog.Warn("codex native project assignment failed", "thread_id", s.CurrentSessionID(), "error", err)
			}
		}
	}
	// 只补空标题，不覆盖用户已经在 Codex 中修改过的标题。
	if name == nil || *name == "" {
		if err := s.requestWithTimeout("thread/name/set", map[string]any{"threadId": s.CurrentSessionID(), "name": title}, nil, 5*time.Second); err != nil {
			s.nativeMetadataErr = errors.Join(s.nativeMetadataErr, err)
			slog.Warn("codex native session naming failed", "thread_id", s.CurrentSessionID(), "error", err)
		}
	}
}

func (s *appServerSession) NativeMetadataError() error { return s.nativeMetadataErr }

func (s *appServerSession) StartupWarning() string {
	if s.nativeMetadataErr == nil {
		return ""
	}
	return "原生会话已创建，但 Codex 项目或标题同步未完成：" + s.nativeMetadataErr.Error()
}
