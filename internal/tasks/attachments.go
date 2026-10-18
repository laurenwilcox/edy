package tasks

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Скриншоты к задаче: хранятся в папке данных Edy (не в проекте) и
// передаются исполнителю, если он видит картинки.

const (
	maxAttachment  = 5 << 20
	maxAttachments = 8
	maxSentImages  = 4 // модели — не больше стольких картинок за раз
)

var attachExt = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}
var attachMime = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}
var safeName = regexp.MustCompile(`[^\p{L}\p{N}._-]+`)

func (m *Manager) attachDir(id string) (string, error) {
	if m.AttachDir == "" {
		return "", errors.New("вложения не настроены")
	}
	if !strings.HasPrefix(id, "task_") || strings.ContainsAny(id, "/\\.") {
		return "", errors.New("некорректная задача")
	}
	return filepath.Join(m.AttachDir, id), nil
}

// Attach сохраняет картинку (data:-URL) к задаче.
func (m *Manager) Attach(id, name, dataURL string) (View, error) {
	rest, ok := strings.CutPrefix(dataURL, "data:")
	meta, data, _ := strings.Cut(rest, ",")
	mediaType, isBase64 := strings.CutSuffix(meta, ";base64")
	ext, known := attachExt[mediaType]
	if !ok || !isBase64 || !known {
		return View{}, errors.New("можно прикрепить картинку PNG, JPEG, GIF или WebP")
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return View{}, errors.New("испорченная картинка")
	}
	if len(raw) > maxAttachment {
		return View{}, fmt.Errorf("картинка больше %d МБ", maxAttachment>>20)
	}
	dir, err := m.attachDir(id)
	if err != nil {
		return View{}, err
	}
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	base = strings.Trim(safeName.ReplaceAllString(base, "-"), "-.")
	if base == "" {
		base = "screenshot"
	}
	if r := []rune(base); len(r) > 40 {
		base = string(r[:40])
	}
	var file string
	_, err = m.update(id, func(x *record) error {
		if len(x.Attachments) >= maxAttachments {
			return fmt.Errorf("не больше %d вложений", maxAttachments)
		}
		file = fmt.Sprintf("%d-%s%s", len(x.Attachments)+1, base, ext)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, file), raw, 0o600); err != nil {
			return err
		}
		x.Attachments = append(x.Attachments, file)
		return nil
	})
	if err != nil {
		return View{}, err
	}
	m.event(id, "", "info", "Прикреплён скриншот "+file)
	return m.Get(id, 0), nil
}

// AttachmentPath возвращает путь к вложению задачи.
func (m *Manager) AttachmentPath(id, file string) (string, error) {
	r, err := m.load(id)
	if err != nil {
		return "", err
	}
	for _, f := range r.Attachments {
		if f == file {
			dir, err := m.attachDir(id)
			return filepath.Join(dir, f), err
		}
	}
	return "", errors.New("вложение не найдено")
}

// RemoveAttachment удаляет вложение.
func (m *Manager) RemoveAttachment(id, file string) (View, error) {
	path, err := m.AttachmentPath(id, file)
	if err != nil {
		return View{}, err
	}
	if _, err := m.update(id, func(x *record) error {
		keep := x.Attachments[:0]
		for _, f := range x.Attachments {
			if f != file {
				keep = append(keep, f)
			}
		}
		x.Attachments = keep
		return nil
	}); err != nil {
		return View{}, err
	}
	os.Remove(path)
	return m.Get(id, 0), nil
}

// images читает вложения задачи как data:-URL для модели.
func (m *Manager) images(r record) []string {
	dir, err := m.attachDir(r.ID)
	if err != nil {
		return nil
	}
	var out []string
	for _, f := range r.Attachments {
		if len(out) == maxSentImages {
			break
		}
		raw, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		out = append(out, "data:"+attachMime[filepath.Ext(f)]+";base64,"+base64.StdEncoding.EncodeToString(raw))
	}
	return out
}
