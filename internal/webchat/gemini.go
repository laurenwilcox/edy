package webchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// Возможности чата на сайте: модели, инструменты, список чатов, подписка и
// лимиты. Сейчас селекторы есть только у Gemini, но код общий: другой сайт
// заработает, если задать ему model_button/tools_button/… в web-sites.json.

// Options — что выбрано в подключении: модель, инструмент и чат. Пустые поля —
// как решит сам сайт (модель по умолчанию, без инструмента, новый чат).
type Options struct {
	Model string `json:"model,omitempty"`
	Tool  string `json:"tool,omitempty"`
	Chat  string `json:"chat,omitempty"` // адрес чата на сайте
}

// Chat — один чат из списка на сайте.
type Chat struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Features — то, что видно на странице чата.
type Features struct {
	Plan    string    `json:"plan"`            // подписка: «Pro», «Ultra», «Бесплатный»…
	Model   string    `json:"model,omitempty"` // модель, выбранная на сайте сейчас
	Models  []string  `json:"models"`
	Tools   []string  `json:"tools"`
	Chats   []Chat    `json:"chats"`
	Limit   string    `json:"limit,omitempty"` // текст предупреждения о лимите, если сайт его показал
	Checked time.Time `json:"checked"`
}

// Usage — сколько запросов Edy отправил через подключение сегодня.
type Usage struct {
	Day   string `json:"day"`
	Used  int    `json:"used"`
	Limit int    `json:"limit"` // 0 — без лимита
}

// ErrNoFeatures — у сайта нет селекторов для моделей и инструментов.
var ErrNoFeatures = errors.New("для этого сайта выбор моделей и инструментов не настроен")

const (
	optionsFile  = "edy-options.json"
	featuresFile = "edy-features.json"
)

// readJSON читает JSON-файл из папки профиля; ошибки дают пустое значение.
func (m *Manager) readJSON(profile, name string, v any) bool {
	if validProfile(profile) != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(m.profileDir(profile), name))
	return err == nil && json.Unmarshal(data, v) == nil
}

func (m *Manager) writeJSON(profile, name string, v any) error {
	if err := validProfile(profile); err != nil {
		return err
	}
	dir := m.profileDir(profile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(v, "", "  ")
	return os.WriteFile(filepath.Join(dir, name), data, 0o600)
}

// Options возвращает сохранённый выбор подключения.
func (m *Manager) Options(profile string) Options {
	var o Options
	m.readJSON(profile, optionsFile, &o)
	return o
}

// SaveOptions запоминает выбор подключения. Чат должен быть на том же сайте.
func (m *Manager) SaveOptions(profile, site string, o Options) error {
	o.Model, o.Tool, o.Chat = strings.TrimSpace(o.Model), strings.TrimSpace(o.Tool), strings.TrimSpace(o.Chat)
	if o.Chat != "" {
		s, err := m.site(site)
		if err != nil {
			return err
		}
		if !sameHost(o.Chat, s.URL) {
			return fmt.Errorf("чат %q не на сайте %s", o.Chat, s.Title)
		}
	}
	return m.writeJSON(profile, optionsFile, o)
}

// CachedFeatures — результат последней проверки возможностей без запуска браузера.
func (m *Manager) CachedFeatures(profile string) (Features, bool) {
	var f Features
	ok := m.readJSON(profile, featuresFile, &f)
	return f, ok
}

// Usage — счётчик запросов Edy за сегодня и дневной лимит сайта.
func (m *Manager) Usage(profile, site string) Usage {
	u := Usage{Day: time.Now().Format("2006-01-02")}
	if s, err := m.site(site); err == nil {
		u.Limit = s.DailyLimit
	}
	m.limitMu.Lock()
	l := m.limits[profile]
	m.limitMu.Unlock()
	if l != nil {
		// Счётчик меняется только под l.mu, но читаем без блокировки: занятое
		// подключение не должно задерживать интерфейс, а неточность в один запрос не страшна.
		if l.day == u.Day {
			u.Used = l.count
		}
	}
	return u
}

// Features открывает сайт, раскрывает меню моделей и инструментов, читает
// список чатов, подписку и предупреждение о лимите. Результат сохраняется.
func (m *Manager) Features(ctx context.Context, c Conn) (Features, error) {
	if err := validProfile(c.Profile); err != nil {
		return Features{}, err
	}
	s, err := m.site(c.Site)
	if err != nil {
		return Features{}, err
	}
	if s.ModelButton == "" && s.ToolsButton == "" && s.ChatLink == "" {
		return Features{}, ErrNoFeatures
	}
	l, err := m.lockProfile(ctx, c.Profile)
	if err != nil {
		return Features{}, err
	}
	defer l.mu.Unlock()
	var f Features
	err = m.withTab(ctx, c, func(tab context.Context, s Site, run runFunc) error {
		if err := m.openChat(ctx, tab, c.Profile, s, run, s.URL); err != nil {
			return err
		}
		var raw struct {
			PlanText string   `json:"planText"`
			Upsell   bool     `json:"upsell"`
			Model    string   `json:"model"`
			Models   []string `json:"models"`
			Tools    []string `json:"tools"`
			Chats    []Chat   `json:"chats"`
			Limit    string   `json:"limit"`
		}
		js := fmt.Sprintf(featuresJS, s.ModelButton, s.ToolsButton, s.MenuItem, s.ChatLink, s.Plan)
		if err := run(40*time.Second, chromedp.Evaluate(js, &raw, awaitPromise)); err != nil {
			return m.fail(tab, c.Profile, fmt.Errorf("не удалось прочитать возможности %s: %w", s.Title, err))
		}
		f = Features{
			Plan:    planName(raw.PlanText, raw.Upsell),
			Model:   raw.Model,
			Models:  orFallback(raw.Models, s.Models),
			Tools:   orFallback(raw.Tools, s.Tools),
			Chats:   raw.Chats,
			Limit:   raw.Limit,
			Checked: time.Now(),
		}
		return nil
	})
	m.record(c.Profile, err)
	if err != nil {
		return Features{}, err
	}
	if f.Chats == nil {
		f.Chats = []Chat{}
	}
	if err := m.writeJSON(c.Profile, featuresFile, f); err != nil && m.Log != nil {
		fmt.Fprintf(m.Log, "webchat: не удалось сохранить возможности %s: %v\n", c.Profile, err)
	}
	return f, nil
}

// openChat открывает адрес и ждёт поле ввода: его нет — не выполнен вход.
func (m *Manager) openChat(ctx context.Context, tab context.Context, profile string, s Site, run runFunc, addr string) error {
	if err := run(60*time.Second, chromedp.Navigate(addr)); err != nil {
		return m.fail(tab, profile, fmt.Errorf("не удалось открыть %s: %w", addr, err))
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var v *string
		if err := run(10*time.Second, chromedp.Evaluate(fmt.Sprintf(inputJS, s.Input), &v)); err == nil && v != nil {
			return nil
		}
		if time.Now().After(deadline) {
			return m.fail(tab, profile, fmt.Errorf("%w в %s: на странице нет поля ввода. Нажмите «Войти» у провайдера в Edy", ErrLoginRequired, s.Title))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// resolve дополняет пустые поля подключения сохранённым выбором.
func (m *Manager) resolve(c Conn) Conn {
	o := m.Options(c.Profile)
	if c.Model == "" || c.Model == "chat" {
		c.Model = o.Model
	}
	if c.Tool == "" {
		c.Tool = o.Tool
	}
	if c.Chat == "" {
		c.Chat = o.Chat
	}
	return c
}

// chatURL — адрес, с которого начинается запрос: выбранный чат или новый.
func chatURL(c Conn, s Site) string {
	if c.Chat != "" && sameHost(c.Chat, s.URL) {
		return c.Chat
	}
	return s.URL
}

// choose выбирает модель и инструмент на открытой странице. Неудача с
// моделью — ошибка (пользователь ждёт ответ именно этой модели), с
// инструментом — тоже: без него запрос «нарисуй» даст текст вместо картинки.
func (m *Manager) choose(tab context.Context, c Conn, s Site, run runFunc) error {
	pick := func(what, button, value string, fallback []string) error {
		if value == "" || value == "chat" || button == "" {
			return nil
		}
		aliases, _ := json.Marshal(aliasesFor(value, fallback))
		var got string
		js := fmt.Sprintf(pickJS, button, s.MenuItem, string(aliases))
		if err := run(20*time.Second, chromedp.Evaluate(js, &got, awaitPromise)); err != nil {
			return m.fail(tab, c.Profile, fmt.Errorf("не удалось выбрать %s %q в %s: %w", what, value, s.Title, err))
		}
		if got == "" {
			return m.fail(tab, c.Profile, fmt.Errorf("в %s нет пункта %s %q — обновите список моделей и инструментов в настройках провайдера", s.Title, what, value))
		}
		return nil
	}
	if err := pick("модели", s.ModelButton, c.Model, s.Models); err != nil {
		return err
	}
	return pick("инструмента", s.ToolsButton, c.Tool, s.Tools)
}

// aliasesFor — варианты названия пункта меню. Значение может быть само списком
// через «|»; если оно совпало с запасным вариантом сайта, берём все его названия
// (на русском и английском интерфейсе пункты называются по-разному).
func aliasesFor(value string, fallback []string) []string {
	out := splitAliases(value)
	for _, f := range fallback {
		group := splitAliases(f)
		for _, a := range out {
			if containsFold(group, a) {
				return append(out, group...)
			}
		}
	}
	return out
}

func splitAliases(s string) []string {
	var out []string
	for _, a := range strings.Split(s, "|") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// orFallback — найденные на странице варианты или запасной список сайта
// (первое название из каждой группы).
func orFallback(found, fallback []string) []string {
	if len(found) > 0 {
		return found
	}
	out := []string{}
	for _, f := range fallback {
		if a := splitAliases(f); len(a) > 0 {
			out = append(out, a[0])
		}
	}
	return out
}

// planName переводит надпись о подписке в короткое название.
func planName(text string, upsell bool) string {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "ultra"):
		return "Ultra"
	case strings.Contains(t, "pro"):
		return "Pro"
	case strings.Contains(t, "plus"):
		return "Plus"
	case upsell:
		return "Бесплатный"
	case strings.TrimSpace(text) != "":
		return strings.TrimSpace(text)
	}
	return "Не определена"
}

func sameHost(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && ua.Scheme == "https" && ua.Host != "" && strings.EqualFold(ua.Host, ub.Host)
}

// menuJS — общие функции для работы с меню на странице.
const menuJS = `
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
const label = (el) => ((el.innerText || el.textContent || "").split("\n").map((s) => s.trim()).filter(Boolean)[0] || "").slice(0, 80);
const firstVis = (sel) => (sel ? [...document.querySelectorAll(sel)].find(vis) || null : null);
const closeMenu = async () => {
  const bd = document.querySelector(".cdk-overlay-backdrop");
  if (bd) bd.click();
  (document.activeElement || document.body).dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", code: "Escape", keyCode: 27, bubbles: true }));
  await wait(300);
};
const openMenu = async (btnSel, itemSel) => {
  const btn = firstVis(btnSel);
  if (!btn) return null;
  btn.click();
  for (let i = 0; i < 15; i++) {
    await wait(200);
    const items = [...document.querySelectorAll(itemSel)].filter(vis);
    if (items.length) return { btn, items };
  }
  return { btn, items: [] };
};
const active = (el) => el.getAttribute("aria-checked") === "true" || el.getAttribute("aria-pressed") === "true" || el.getAttribute("aria-selected") === "true" || /\b(selected|active|is-selected)\b/.test(el.className || "");
`

// featuresJS — аргументы: кнопка моделей, кнопка инструментов, пункт меню,
// ссылка на чат, подписка.
const featuresJS = `(async () => {` + pageJS + menuJS + `
  const out = { planText: "", upsell: false, model: "", models: [], tools: [], chats: [], limit: "" };
  const modelBtn = %[1]q, toolsBtn = %[2]q, itemSel = %[3]q, chatSel = %[4]q, planSel = %[5]q;
  const uniq = (a) => [...new Set(a.filter(Boolean))];
  if (modelBtn) {
    const b = firstVis(modelBtn);
    if (b) out.model = label(b);
    const m = await openMenu(modelBtn, itemSel);
    if (m) out.models = uniq(m.items.map(label));
    await closeMenu();
  }
  if (toolsBtn) {
    const m = await openMenu(toolsBtn, itemSel);
    if (m) out.tools = uniq(m.items.map(label));
    await closeMenu();
  }
  if (chatSel) {
    const seen = new Set();
    for (const el of document.querySelectorAll(chatSel)) {
      const a = el.tagName === "A" ? el : el.closest("a") || el.querySelector("a");
      const href = a && a.href;
      const title = label(el);
      if (!href || !title || seen.has(href)) continue;
      seen.add(href);
      out.chats.push({ title, url: href });
      if (out.chats.length >= 100) break;
    }
  }
  if (planSel) {
    for (const el of document.querySelectorAll(planSel)) {
      const t = (el.innerText || el.getAttribute("aria-label") || "").trim();
      if (/upgrade|улучш|попроб|try/i.test(t) || el.tagName === "UPSELL-BUTTON") out.upsell = true;
      else if (t && !out.planText) out.planText = t.slice(0, 40);
    }
  }
  const text = document.body ? document.body.innerText : "";
  for (const line of text.split("\n")) {
    if (/(limit|лимит)/i.test(line) && /(reach|достиг|исчерп|reset|сброс|until|до )/i.test(line)) {
      out.limit = line.trim().slice(0, 300);
      break;
    }
  }
  return out;
})()`

// pickJS — аргументы: кнопка меню, пункт меню, варианты названия (JSON).
// Возвращает текст выбранного пункта или пустую строку.
const pickJS = `(async () => {` + pageJS + menuJS + `
  const aliases = %[3]s.map((a) => a.toLowerCase());
  const btn = firstVis(%[1]q);
  if (btn && aliases.some((a) => label(btn).toLowerCase().includes(a))) return label(btn);
  const m = await openMenu(%[1]q, %[2]q);
  if (!m) return "";
  const item = m.items.find((el) => aliases.some((a) => label(el).toLowerCase().includes(a)));
  if (!item) { await closeMenu(); return ""; }
  const name = label(item);
  if (!active(item)) item.click(); else await closeMenu();
  await wait(500);
  return name;
})()`
