package omniroute

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Утилиты, которым нужно явно разрешить чтение папки с картинками.
var addDirFlag = map[string]string{"claude": "--add-dir"}

var imageExt = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}

// materializeImages заменяет картинки в сообщениях на файлы: CLI-утилита
// получает путь и открывает файл сама (Claude Code и другие умеют читать
// изображения). Возвращает новые сообщения, папку с файлами (пусто — картинок
// не было) и функцию уборки.
func materializeImages(msgs []Message) ([]Message, string, func(), error) {
	dir := ""
	cleanup := func() {
		if dir != "" {
			os.RemoveAll(dir)
		}
	}
	out := make([]Message, len(msgs))
	n := 0
	for i, m := range msgs {
		out[i] = m
		parts, err := m.Parts()
		if err != nil {
			cleanup()
			return nil, "", nil, err
		}
		changed := false
		for j, p := range parts {
			switch p.Type {
			case "input_audio":
				cleanup()
				return nil, "", nil, errors.New("CLI-утилиты не принимают звук — выберите модель с тегом «Звук»")
			case "image_url":
				if p.ImageURL == nil {
					continue
				}
				changed = true
				url := p.ImageURL.URL
				rest, ok := strings.CutPrefix(url, "data:")
				if !ok {
					parts[j] = ContentPart{Type: "text", Text: "[Изображение по ссылке: " + url + "]"}
					continue
				}
				meta, data, _ := strings.Cut(rest, ",")
				mediaType, isBase64 := strings.CutSuffix(meta, ";base64")
				ext, known := imageExt[mediaType]
				if !isBase64 || !known {
					cleanup()
					return nil, "", nil, fmt.Errorf("картинка %q: нужен PNG, JPEG, GIF или WebP в base64", mediaType)
				}
				raw, err := base64.StdEncoding.DecodeString(data)
				if err != nil {
					cleanup()
					return nil, "", nil, errors.New("картинка: испорченный base64")
				}
				if dir == "" {
					if dir, err = os.MkdirTemp("", "edy-images-"); err != nil {
						return nil, "", nil, err
					}
				}
				n++
				path := filepath.Join(dir, fmt.Sprintf("image-%d%s", n, ext))
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					cleanup()
					return nil, "", nil, err
				}
				parts[j] = ContentPart{Type: "text", Text: "[Изображение: " + path + " — открой файл, чтобы посмотреть]"}
			}
		}
		if changed {
			var b strings.Builder
			for _, p := range parts {
				if p.Type == "text" {
					b.WriteString(p.Text)
					b.WriteString("\n")
				}
			}
			out[i].Content, _ = json.Marshal(strings.TrimSpace(b.String()))
		}
	}
	return out, dir, cleanup, nil
}
