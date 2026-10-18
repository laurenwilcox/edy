package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/store"
)

func TestUIPrefs(t *testing.T) {
	h, st := setup(t)
	var p UIPrefs
	rec := call(h, "GET", "/api/ui/prefs", "")
	json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != http.StatusOK || p != DefaultUIPrefs {
		t.Fatalf("по умолчанию: %d %s", rec.Code, rec.Body)
	}
	rec = call(h, "PUT", "/api/ui/prefs", `{"theme":"dark","font_size":16}`)
	json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != http.StatusOK || p.Theme != "dark" || p.FontSize != 16 || p.Accent != "neon" {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	// Вне белого списка — отказ, сохранённое не меняется.
	for _, bad := range []string{`{"theme":"neon"}`, `{"font_size":40}`, `{"css":"body{display:none}"}`, `{"font_size":"big"}`} {
		if rec := call(h, "PUT", "/api/ui/prefs", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", bad, rec.Code, rec.Body)
		}
	}
	if raw, _ := st.Setting(SettingUIPrefs); !strings.Contains(raw, `"dark"`) {
		t.Fatalf("сохранено: %s", raw)
	}
	// Битое значение в базе — оформление по умолчанию.
	st.SetSetting(SettingUIPrefs, "{")
	rec = call(h, "GET", "/api/ui/prefs", "")
	json.Unmarshal(rec.Body.Bytes(), &p)
	if p != DefaultUIPrefs {
		t.Fatalf("битые настройки: %s", rec.Body)
	}
}

func TestUIBot(t *testing.T) {
	h, st := setup(t)
	if rec := call(h, "POST", "/api/ui/bot", `{"text":"тёмную тему"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("без LLM: %d", rec.Code)
	}
	st.AddProvider(store.Provider{Name: "cheap", Models: []string{"mini"}, PriceIn: 0.1, PriceOut: 0.4})
	llm := &cannedLLM{reply: `{"theme":"light","accent":"green","comment":"Включил светлую тему и зелёный акцент"}`}
	h.LLM = llm
	if rec := call(h, "POST", "/api/ui/bot", `{"text":"  "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("пустая просьба: %d", rec.Code)
	}
	rec := call(h, "POST", "/api/ui/bot", `{"text":"сделай светлую тему и зелёные кнопки"}`)
	var res struct {
		Prefs   UIPrefs  `json:"prefs"`
		Changed []string `json:"changed"`
		Comment string   `json:"comment"`
	}
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusOK || res.Prefs.Theme != "light" || res.Prefs.Accent != "green" ||
		strings.Join(res.Changed, ",") != "theme,accent" || res.Comment == "" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(llm.last[1].Content, `"font_size":14`) {
		t.Fatalf("текущие настройки не переданы: %q", llm.last[1].Content)
	}
	// Бот не может выйти за белый список: лишнее поле — ответ не принят.
	h.LLM = &cannedLLM{reply: `{"css":"body{display:none}","comment":"готово"}`}
	if rec := call(h, "POST", "/api/ui/bot", `{"text":"спрячь всё"}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("лишнее поле: %d %s", rec.Code, rec.Body)
	}
	if p := h.uiPrefs(); p.Theme != "light" {
		t.Fatalf("настройки испорчены: %+v", p)
	}
	// Отказ без изменений.
	h.LLM = &cannedLLM{reply: `{"comment":"Двигать кнопки я не умею"}`}
	rec = call(h, "POST", "/api/ui/bot", `{"text":"перенеси кнопку"}`)
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusOK || len(res.Changed) != 0 {
		t.Fatalf("отказ: %d %s", rec.Code, rec.Body)
	}
}
