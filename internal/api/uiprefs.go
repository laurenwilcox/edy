package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/laurenwilcox/name/internal/micro"
)

// SettingUIPrefs — настройки оформления интерфейса (JSON UIPrefs).
const SettingUIPrefs = "ui.prefs"

// UIPrefs — безопасный набор настроек интерфейса. UI-бот меняет только их:
// CSS и JS интерфейса не трогает, поэтому «сломать» IDE нельзя — любое
// значение вне белого списка отклоняется.
type UIPrefs struct {
	Theme      string `json:"theme"`
	Accent     string `json:"accent"`
	FontSize   int    `json:"font_size"`
	Font       string `json:"font"`
	Sidebar    string `json:"sidebar"`
	Animations string `json:"animations"`
}

// DefaultUIPrefs — оформление по умолчанию.
var DefaultUIPrefs = UIPrefs{Theme: "system", Accent: "indigo", FontSize: 14, Font: "system", Sidebar: "left", Animations: "on"}

func num(x float64) *float64 { return &x }

// uiPrefFields — белый список: схема и для проверки PUT, и для ответа UI-бота.
var uiPrefFields = []micro.Field{
	{Name: "theme", Type: micro.TypeString, Enum: []string{"system", "light", "dark"}, Description: "тема: system — как в macOS, light — светлая, dark — тёмная"},
	{Name: "accent", Type: micro.TypeString, Enum: []string{"indigo", "blue", "green", "orange", "pink", "red"}, Description: "акцентный цвет кнопок и выделений"},
	{Name: "font_size", Type: micro.TypeInt, Min: num(12), Max: num(18), Description: "размер шрифта в пикселях"},
	{Name: "font", Type: micro.TypeString, Enum: []string{"system", "serif", "mono"}, Description: "шрифт интерфейса: системный, с засечками, моноширинный"},
	{Name: "sidebar", Type: micro.TypeString, Enum: []string{"left", "right"}, Description: "с какой стороны меню"},
	{Name: "animations", Type: micro.TypeString, Enum: []string{"on", "off"}, Description: "анимации и живой фон"},
}

// checkUIPrefs проверяет настройки по белому списку.
func checkUIPrefs(p UIPrefs) error {
	raw, _ := json.Marshal(p)
	_, err := micro.Validate(string(raw), uiPrefFields)
	return err
}

// uiPrefs читает сохранённые настройки; битые или отсутствующие — по умолчанию.
func (h *Handler) uiPrefs() UIPrefs {
	p := DefaultUIPrefs
	raw, err := h.Store.Setting(SettingUIPrefs)
	if err != nil || json.Unmarshal([]byte(raw), &p) != nil || checkUIPrefs(p) != nil {
		return DefaultUIPrefs
	}
	return p
}

// mergeUIPrefs накладывает изменения поверх p и проверяет результат.
func mergeUIPrefs(p UIPrefs, patch map[string]any) (UIPrefs, error) {
	cur := map[string]any{}
	raw, _ := json.Marshal(p)
	json.Unmarshal(raw, &cur)
	for k, v := range patch {
		cur[k] = v
	}
	raw, _ = json.Marshal(cur)
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var out UIPrefs
	if err := dec.Decode(&out); err != nil {
		return p, errors.New("неизвестная или неверная настройка: " + err.Error())
	}
	if err := checkUIPrefs(out); err != nil {
		return p, err
	}
	return out, nil
}

// changedUIPrefs перечисляет изменившиеся настройки.
func changedUIPrefs(a, b UIPrefs) []string {
	var am, bm map[string]any
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	json.Unmarshal(ra, &am)
	json.Unmarshal(rb, &bm)
	changed := []string{}
	for _, f := range uiPrefFields {
		if am[f.Name] != bm[f.Name] {
			changed = append(changed, f.Name)
		}
	}
	return changed
}

func (h *Handler) saveUIPrefs(p UIPrefs) error {
	raw, _ := json.Marshal(p)
	return h.Store.SetSetting(SettingUIPrefs, string(raw))
}

func (h *Handler) uiPrefsHandler(w http.ResponseWriter, r *http.Request) {
	p := h.uiPrefs()
	if r.Method == http.MethodPut {
		var patch map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			fail(w, http.StatusBadRequest, "некорректный JSON")
			return
		}
		next, err := mergeUIPrefs(p, patch)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := h.saveUIPrefs(next); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		p = next
	}
	reply(w, p)
}

// uiBot — UI-бот: микро-агент переводит просьбу пользователя в изменения
// белого списка настроек. Всё, что вне списка, он сделать не может.
func (h *Handler) uiBot(w http.ResponseWriter, r *http.Request) {
	if h.LLM == nil {
		fail(w, http.StatusServiceUnavailable, "модели недоступны")
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		fail(w, http.StatusBadRequest, "напишите, что изменить в оформлении")
		return
	}
	cur := h.uiPrefs()
	curRaw, _ := json.Marshal(cur)
	fields := append([]micro.Field{}, uiPrefFields...)
	fields = append(fields, micro.Field{Name: "comment", Type: micro.TypeString, Required: true,
		Description: "одна короткая фраза пользователю по-русски: что изменено, или почему просьбу выполнить нельзя"})
	res, err := h.runMicro(r.Context(), "", micro.Task{
		Name: "ui-bot",
		Prompt: "Ты — UI-бот IDE Edy. Ты меняешь только настройки оформления из списка полей. " +
			"Включи в ответ только те настройки, которые нужно изменить по просьбе пользователя. " +
			"Код, CSS, расположение отдельных кнопок и всё прочее вне списка менять нельзя — тогда объясни это в comment.\n\n" +
			"Просьба пользователя: " + req.Text,
		Input:  "Текущие настройки: " + string(curRaw),
		Fields: fields,
	})
	if err != nil {
		microFail(w, err)
		return
	}
	comment, _ := res.Data["comment"].(string)
	delete(res.Data, "comment")
	next, err := mergeUIPrefs(cur, res.Data)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := h.saveUIPrefs(next); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, map[string]any{"prefs": next, "changed": changedUIPrefs(cur, next), "comment": comment})
}
