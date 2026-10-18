# 05. HTTP API Edy (`internal/api`) — полный справочник

Этот документ — справочник по всем маршрутам `/api/...` и `/mcp`, которые обслуживает пакет
`internal/api`. Маршруты OmniRoute (`/v1/...`, `/healthz`) описаны в [01-omniroute.md](01-omniroute.md),
внутреннее устройство MCP-сервера (список инструментов) — в [04-mcp.md](04-mcp.md), модель данных —
в [06-store.md](06-store.md), интерфейс, который вызывает эти маршруты, — в [07-ui.md](07-ui.md).

Все ссылки вида `файл:строка` даны относительно `internal/api/`, если не указано иное.

---

## 1. Общая схема

### 1.1. Где монтируется API

Весь HTTP-сервер собирается в `internal/app/app.go:166-171`:

```go
mux := http.NewServeMux()
mux.Handle("/v1/", router)      // OmniRoute (см. 01-omniroute.md)
mux.Handle("/healthz", router)  // OmniRoute, без токена
mux.Handle("/api/", h)          // *api.Handler — этот документ
mux.Handle("/mcp", h.MCP())     // MCP Streamable HTTP — раздел 16
mux.Handle("/", ui.Handler())   // статический интерфейс — раздел 2
```

Сервер слушает адрес, переданный в `app.Start` (по умолчанию `127.0.0.1:11435`, см. 08-app-build.md),
`ReadHeaderTimeout: 10s` (`internal/app/app.go:174`). Общих таймаутов на запись нет: длинные операции
(установка утилиты, клонирование, Оркестратор) ограничивают себя собственным `context.WithTimeout`.

### 1.2. Структура `Handler`

`api.go:46-77`. Поля:

| Поле | Тип | Назначение | Кто заполняет |
|---|---|---|---|
| `Store` | `*store.Store` | база SQLite | `api.New` |
| `Token` | `string` | Bearer-токен OmniRoute (он же токен API) | `api.New` |
| `Addr` | `string` | фактический адрес `host:port`, для `/api/info` и URL привязок MCP | `api.New` (`ln.Addr().String()`) |
| `OpenURL` | `func(string) error` | открыть ссылку в браузере (`open` на macOS, `xdg-open` иначе; `api.go:664`) | `api.New`, подменяется в тестах |
| `FindExecutable` | `func(name) (string, error)` | поиск CLI-утилиты (`omniroute.FindExecutable`) | `api.New` |
| `Install` | `func(ctx, cmd) (out, err)` | запуск команды установки через `/bin/bash -c` (`runInstall`, `api.go:672`) | `api.New` |
| `ListModels` | `func(ctx, Provider, key) ([]string, error)` | запрос списка моделей у провайдера (`omniroute.ListModels`) | `api.New` |
| `Web` | `WebManager` | менеджер браузеров веб-чатов; `nil` — веб-чаты выключены | `app.Start` (`webchat.Manager`) |
| `LLM` | `agent.LLM` | клиент OmniRoute для Оркестратора (`X-Edy-Client: orchestrator`, in-process) | `app.Start:114` |
| `Tasks` | `*tasks.Manager` | доска задач и очередь исполнителей; `nil` — задачи недоступны | `app.Start:115` |
| `ReposDir` | `string` | куда клонировать репозитории (`<Home>/repos`) | `app.Start:157` |
| `Home` | `string` | папка данных Edy (общие скиллы в `<Home>/skills`) | `app.Start:158` |
| `Notify` | `notify.Notifier` | системное уведомление macOS (`notify.System`) | `api.New` |
| `Semantic` | `*semantic.Engine` | поиск по смыслу; `nil` — только поиск по словам | `app.Start:119` |
| `mux` | `*http.ServeMux` | внутренний роутер `/api/*` | `api.New` |
| `mcpM`, `mcpS`, `mcpB` | `sync.Mutex`, `map[string]*mcpSession`, `map[string]*mcpBinding` | сессии и привязки MCP в памяти | раздел 16 |

Интерфейс `WebManager` (`api.go:33-40`): `Sites()`, `Login(ctx, Conn)`, `Check(ctx, Conn)`,
`Status(profile)`, `RemoveProfile(name)`, `BrowserInstalled(kind)`. Реализация — `webchat.Manager`.

Конструктор `New(st, token, addr)` — `api.go:79-113`: заполняет значения по умолчанию, регистрирует все
маршруты (часть — через `register*Routes` в соседних файлах) и сохраняет mux.

### 1.3. Маршрутизация

Используется стандартный `net/http.ServeMux` Go 1.22+ (в `go.mod` — `go 1.26.0`) с шаблонами вида
`"METHOD /path/{param}"`. Значения параметров берутся через `r.PathValue("name")`. Особенности:

* Более конкретный шаблон побеждает: `POST /api/tasks/{id}/attachments` и `POST /api/tasks/{id}/revert-file`
  важнее, чем `POST /api/tasks/{id}/{action}`; `POST /api/git/clone` важнее, чем `POST /api/git/{action}`.
* Если путь совпал, а метод нет, `ServeMux` сам отвечает `405 Method Not Allowed` **текстом** (не JSON)
  с заголовком `Allow`. Несуществующий путь — `404 page not found` текстом. Интерфейс в этом случае покажет
  «Ошибка 405/404» (`index.html`, функция `api()`).
* Запрос `/api` без слэша внешний mux перенаправит на `/api/` (301) — практического значения не имеет.

### 1.4. Аутентификация

**Все** маршруты `/api/*` требуют заголовок `Authorization: Bearer <token>`. Проверка — в
`Handler.ServeHTTP` (`api.go:115-122`), **до** маршрутизации:

```go
got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(h.Token)) != 1 {
    fail(w, http.StatusUnauthorized, "нужен токен OmniRoute")
    return
}
```

* Сравнение постоянное по времени (`crypto/subtle`).
* Токен — тот же, что у OmniRoute: `edy-` + 48 hex-символов (`omniroute.NewToken`, `internal/omniroute/server.go:77`).
  Создаётся при первом запуске (`app.EnsureToken`, `internal/app/app.go:62`), хранится в настройке
  `router_token` и копируется в файл `<Home>/router.token` (права `0600`).
* Зачем: страница любого сайта в браузере пользователя может слать запросы на `127.0.0.1`. Без токена
  она могла бы управлять Edy (CSRF через localhost). Заголовок `Authorization` нельзя отправить с чужого
  сайта без CORS-preflight, а CORS-заголовков Edy не выдаёт.
* Ответ без токена: `401 {"error":"нужен токен OmniRoute"}`. Неверный путь без токена тоже даёт 401
  (проверка идёт раньше маршрутизации).
* `/mcp` проверяет токен так же, отдельно (`mcpapi.go:53-57`).

**Публичные (без токена) маршруты:** только статический интерфейс `/` (любой путь, не попавший в
`/v1/`, `/api/`, `/mcp`, `/healthz`) и `/healthz` (OmniRoute).

**Как токен попадает в интерфейс.** `app.Server.URL()` (`internal/app/app.go:88`) возвращает
`http://<addr>/#token=<token>`. `#`-часть браузер не отправляет на сервер и не пишет в логи прокси.
Скрипт интерфейса (`internal/ui/static/index.html`, ~строка 874) читает `location.hash`, кладёт токен в
`sessionStorage["edy-token"]` (и флаг `edy-app=1`, если в hash есть `app=1` — признак окна Wails),
стирает hash через `history.replaceState` и дальше добавляет `Authorization: Bearer …` к каждому
запросу (`api()` ~строка 914). Вложения-картинки грузятся тоже `fetch` с заголовком, а не через `<img src>`.

### 1.5. Формат ответов и ошибок

Вспомогательные функции (`api.go:717-727`):

| Функция | Что делает |
|---|---|
| `reply(w, v)` | `replyStatus(w, 200, v)` |
| `replyStatus(w, status, v)` | `Content-Type: application/json`, код `status`, тело — `json.Encoder.Encode(v)` (с переводом строки в конце) |
| `fail(w, status, msg)` | `replyStatus(w, status, {"error": msg})` |

* Ошибка всегда имеет вид `{"error": "<текст на русском>"}`. Исключения: `409` при `POST /api/providers`
  (`needInstall`, дополнительно `tool` и `install`) и `502` при `POST /api/install` (дополнительно `output`).
* Успех без тела — `204 No Content` (`w.WriteHeader(http.StatusNoContent)`).
* Тело запроса — JSON, читается `json.NewDecoder(r.Body).Decode`. Неизвестные поля молча игнорируются.
  Невалидный JSON почти везде даёт `400 {"error":"некорректный JSON"}`.
* Ограничение размера тела есть только у вложений (`8 МБ`, `projects.go:835`) и `/mcp` (`8 МБ`, `mcpapi.go:83`).
  Остальные маршруты размер не ограничивают (доступ всё равно только с токеном).
* Время (`time.Time`) сериализуется в RFC 3339.

### 1.6. Проверка проекта: `knownProject`

`projects.go:165-173`. Почти все маршруты, принимающие путь к проекту, сначала вызывают
`h.knownProject(w, path)`: путь должен **точно** (строковым сравнением) совпасть с одним из путей в
настройке `projects` (раздел 6). Иначе — `404 {"error":"проект не найден — добавьте его в разделе «Проект»"}`
и обработчик завершается. Так API не даёт читать/менять произвольные папки на диске.

Маршруты, которые **не** проверяют проект (обратите внимание): `PUT /api/memory/{id}`,
`DELETE /api/memory/{id}` (но фильтруют по `project` в SQL), `DELETE /api/team/{id}`,
все `/api/tasks/{id}/...` (работают по id задачи), `GET /api/agents*`.

### 1.7. Модели в виде «провайдер/модель»

`splitModelName(m)` (`projects.go:943`) делит строку по **первому** `/`: `openrouter/anthropic/claude-x` →
провайдер `openrouter`, модель `anthropic/claude-x`. `ok=false`, если `/` нет или одна из частей пустая.
`checkModels(w, list)` (`projects.go:434`) проверяет каждую модель: формат и существование провайдера
в базе (`Store.Provider`). При первой неизвестной — `400 {"error":"неизвестная модель <m>"}`. Наличие самой
модели в списке `models` провайдера **не** проверяется.

---

## 2. Статический интерфейс (`internal/ui`)

`internal/ui/ui.go`. Файлы встраиваются в бинарник через `//go:embed static` (сейчас это один файл
`internal/ui/static/index.html`). `ui.Handler()` (`ui.go:14`) отдаёт `fs.Sub(static, "static")`
через `http.FileServer` и на **каждый** ответ ставит заголовки:

```
X-Frame-Options: DENY
Content-Security-Policy: default-src 'self'; script-src 'self' 'unsafe-inline';
  style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; media-src 'self' data: blob:;
  frame-ancestors 'none'
```

* Страницу нельзя встроить во фрейм (clickjacking): `X-Frame-Options` + `frame-ancestors 'none'`.
* Скрипты и стили только свои и инлайновые (весь интерфейс — один HTML с `<script>` и `<style>`),
  внешних CDN нет. `connect-src` не задан явно → наследует `default-src 'self'`: интерфейс может
  ходить только на тот же origin.
* `img-src`/`media-src` разрешают `data:` и `blob:` — для вложений-скриншотов (их грузят через `fetch`
  с токеном и показывают как `blob:`-URL).
* Аутентификации нет: сама страница не содержит секретов, все данные приходят через `/api` с токеном.
* `http.FileServer` отдаёт `index.html` на `/`, а несуществующие пути — `404`.

---

## 3. Сводная таблица маршрутов

| Метод и путь | Обработчик | Раздел |
|---|---|---|
| `GET /api/info` | `info` api.go:124 | 15 |
| `GET /api/presets` | `presets` api.go:173 | 5 |
| `GET /api/providers` | `listProviders` api.go:340 | 4 |
| `POST /api/providers` | `addProvider` api.go:385 | 4 |
| `DELETE /api/providers/{name}` | `deleteProvider` api.go:474 | 4 |
| `POST /api/providers/{name}/keys` | `addKey` api.go:493 | 4 |
| `GET /api/providers/{name}/remote-models` | `remoteModels` api.go:202 | 4 |
| `PUT /api/providers/{name}/models` | `setModels` api.go:230 | 4 |
| `PUT /api/providers/{name}/caps` | `setCaps` api.go:315 | 4 |
| `GET /api/caps` | `capsCatalog` api.go:310 | 4 |
| `DELETE /api/keys/{id}` | `deleteKey` api.go:523 | 4 |
| `POST /api/install` | `install` api.go:540 | 5 |
| `GET /api/prefs` | `prefs` api.go:276 | 15 |
| `PUT /api/prefs` | `setPrefs` api.go:281 | 15 |
| `POST /api/notify/test` | `testNotify` api.go:302 | 15 |
| `POST /api/web/login` | `webLogin` api.go:592 | 7 |
| `POST /api/web/check` | `webCheck` api.go:607 | 7 |
| `GET /api/web/status` | `webStatus` api.go:618 | 7 |
| `GET /api/web/browsers` | `webBrowsers` api.go:635 | 7 |
| `POST /api/open` | `openURL` api.go:644 | 15 |
| `GET /api/usage` | `usage` api.go:683 | 6 |
| `PUT /api/budget` | `setBudget` api.go:702 | 6 |
| `GET /api/stats/models` | `modelStats` projects.go:370 | 6 |
| `GET /api/semantic` | `semanticStatus` semanticapi.go:29 | 14 |
| `PUT /api/semantic` | `setSemantic` semanticapi.go:44 | 14 |
| `POST /api/semantic/index` | `semanticIndex` semanticapi.go:63 | 14 |
| `GET /api/semantic/search` | `semanticSearch` semanticapi.go:88 | 14 |
| `GET /api/projects` | `listProjects` projects.go:102 | 8 |
| `POST /api/projects` | `addProject` projects.go:123 | 8 |
| `DELETE /api/projects` | `removeProject` projects.go:149 | 8 |
| `GET /api/projects/checkpoints` | `checkpoints` projects.go:175 | 8 |
| `POST /api/projects/restore` | `restore` projects.go:192 | 8 |
| `GET /api/projects/context` | `projectContext` projects.go:216 | 8 |
| `GET /api/projects/files` | `projectFiles` projects.go:912 | 8 |
| `GET /api/projects/file` | `projectFile` projects.go:929 | 8 |
| `POST /api/orchestrator` | `orchestrate` projects.go:233 | 9 |
| `GET /api/tasks` | `listTasks` projects.go:421 | 10 |
| `POST /api/tasks` | `startTask` projects.go:451 | 10 |
| `POST /api/board` | `createTask` projects.go:581 | 10 |
| `POST /api/board/issue` | `importIssue` projects.go:604 | 10 |
| `GET /api/templates` | `templates` projects.go:680 | 10 |
| `POST /api/templates` | `addTemplate` projects.go:688 | 10 |
| `DELETE /api/templates` | `deleteTemplate` projects.go:720 | 10 |
| `GET /api/tasks/{id}` | `getTask` projects.go:733 | 10 |
| `PUT /api/tasks/{id}` | `editTask` projects.go:747 | 10 |
| `DELETE /api/tasks/{id}` | `deleteTask` projects.go:784 | 10 |
| `GET /api/tasks/{id}/patch` | `taskPatch` projects.go:878 | 10 |
| `POST /api/tasks/{id}/revert-file` | `revertFile` projects.go:891 | 10 |
| `POST /api/tasks/{id}/attachments` | `addAttachment` projects.go:830 | 10 |
| `GET /api/tasks/{id}/attachments/{file}` | `getAttachment` projects.go:851 | 10 |
| `DELETE /api/tasks/{id}/attachments/{file}` | `deleteAttachment` projects.go:865 | 10 |
| `POST /api/tasks/{id}/{action}` | `taskAction` projects.go:796 | 10 |
| `GET /api/git/accounts` | `gitAccounts` gitapi.go:34 | 11 |
| `POST /api/git/accounts` | `addGitAccount` gitapi.go:43 | 11 |
| `DELETE /api/git/accounts/{id}` | `deleteGitAccount` gitapi.go:93 | 11 |
| `POST /api/git/clone` | `gitClone` gitapi.go:117 | 11 |
| `GET /api/git/info` | `gitInfo` gitapi.go:173 | 11 |
| `PUT /api/git/settings` | `gitSettings` gitapi.go:186 | 11 |
| `POST /api/git/{action}` | `gitRemoteAction` gitapi.go:248 | 11 |
| `GET /api/agents` | `agents` gitapi.go:317 | 12 |
| `GET /api/agents/{id}` | `agentDetail` gitapi.go:326 | 12 |
| `GET /api/skills` | `listSkills` mcpapi.go:292 | 13.1 |
| `POST /api/skills` | `saveSkill` mcpapi.go:310 | 13.1 |
| `DELETE /api/skills` | `deleteSkill` mcpapi.go:334 | 13.1 |
| `GET /api/mcp-servers` | `listMCPServers` mcpcatalog.go:75 | 13.2 |
| `POST /api/mcp-servers` | `saveMCPServer` mcpcatalog.go:89 | 13.2 |
| `PUT /api/mcp-servers/{name}/enabled` | `toggleMCPServer` mcpcatalog.go:151 | 13.2 |
| `DELETE /api/mcp-servers/{name}` | `deleteMCPServer` mcpcatalog.go:169 | 13.2 |
| `GET /api/memory` | `listMemory` memoryapi.go:23 | 13.3 |
| `POST /api/memory` | `addMemory` memoryapi.go:56 | 13.3 |
| `PUT /api/memory/{id}` | `pinMemory` memoryapi.go:99 | 13.3 |
| `DELETE /api/memory/{id}` | `deleteMemory` memoryapi.go:119 | 13.3 |
| `GET /api/team` | `listTeam` teamapi.go:54 | 13.4 |
| `POST /api/team` | `saveTeamAgent` teamapi.go:79 | 13.4 |
| `DELETE /api/team/{id}` | `deleteTeamAgent` teamapi.go:128 | 13.4 |
| `POST /mcp`, `DELETE /mcp` | `serveMCP` mcpapi.go:52 | 16 |

Регистрация: базовые маршруты — `api.go:82-104`; `semanticRoutes` — `semanticapi.go:13-18`;
`registerProjectRoutes` — `projects.go:29-56`; `registerGitRoutes` — `gitapi.go:22-32`;
`registerSkillRoutes` — `mcpapi.go:278-282`; `registerMCPCatalogRoutes` — `mcpcatalog.go:60-65`;
`registerMemoryRoutes` — `memoryapi.go:16-21`; `registerTeamRoutes` — `teamapi.go:26-30`.
`/mcp` регистрируется не здесь, а в `internal/app/app.go:170` через `h.MCP()`.

---

## 4. Провайдеры и ключи

Провайдер — источник моделей для OmniRoute. Имя провайдера — префикс модели (`<name>/<model>`).
Типы (`store.Kind*`, `internal/store/store.go:18-23`): `openai`, `anthropic`, `cli`, `web`.

### 4.1. `GET /api/providers` — список провайдеров

`listProviders`, `api.go:340`. Без параметров.

Ответ `200` — массив `providerJSON` (`api.go:253-271`), отсортирован по имени (SQL `ORDER BY name`):

| Поле | Тип | Значение |
|---|---|---|
| `name` | string | имя |
| `kind` | string | `openai` / `anthropic` / `cli` / `web` |
| `base_url` | string | адрес API (для `openai`/`anthropic`) |
| `models` | string[] | подключённые модели (никогда не `null`) |
| `price_in`, `price_out` | number | USD за 1M входных/выходных токенов |
| `command` | string[] \| null | команда запуска CLI с подстановками `{model}`, `{prompt}` |
| `site` | string | веб-чат (`chatgpt`, `claude`, `gemini`…) |
| `browser` | string | `chromium` или `chrome` |
| `keys` | `KeyInfo[]` | `[{id:int, label:string, disabled:bool}]` — **без секретов** |
| `mcp` | bool | у провайдера включён тег `mcp` (`omniroute.HasCap(p, CapMCP)`) |
| `caps` | string[] | действующие теги возможностей (`omniroute.Caps(p)`: заданные вручную или автоопределённые) |
| `caps_auto` | bool | `true`, если теги не заданы вручную (`p.Caps == nil`) |
| `can_mcp` | bool | `kind == cli` и утилита умеет подключать MCP (`omniroute.SupportsMCP(command)`) |

Ошибки: `500` — ошибка БД.

### 4.2. `POST /api/providers` — добавить провайдера

`addProvider`, `api.go:385`. Тело `addProviderReq` (`api.go:365-376`):

| Поле | Тип | Обязательно | Комментарий |
|---|---|---|---|
| `name` | string | да | обрезается; не может быть пустым и содержать `/` |
| `kind` | string | да | `openai` / `anthropic` / `cli` / `web` |
| `base_url` | string | для API | обрезается; не валидируется |
| `models` | string[] | нет | пустые строки выкидываются |
| `price_in`, `price_out` | number | нет | ≥ 0 |
| `preset` | string | для `cli` (или `command`) | ключ `omniroute.CLIPresets` |
| `command` | string[] | для `cli` без preset | должна содержать `{prompt}` |
| `site` | string | для `web` | ключ из `Web.Sites()` |
| `browser` | string | для `web` | `chromium` (по умолчанию) или `chrome` |

Логика по типам:
* `openai`, `anthropic` — без доп. проверок.
* `cli` с `preset`: профиль ищется в `omniroute.CLIPresets`; бинарник `Command[0]` ищется через
  `FindExecutable`. Если не найден — **`409`** с телом `needInstall` (`api.go:379-383`):
  `{"error":"утилита \"x\" не установлена","tool":"<preset>","install":"<команда установки>"}` —
  интерфейс предлагает установить через `POST /api/install`. Если найден — в `command` пишется
  абсолютный путь + остальные аргументы профиля.
* `cli` с `command`: в склейке аргументов должна быть подстановка `{prompt}`, иначе `400`.
* `web`: нужен `h.Web != nil` (иначе `400 "веб-чаты выключены"`), `site` из каталога сайтов, браузер
  `chromium`/`chrome`. Если моделей нет — подставляется `["chat"]` (модель выбирается на самом сайте).

Ответ `201 {"name":"<name>"}`.

Ошибки: `400` (некорректный JSON; «имя провайдера обязательно и не может содержать «/»»; «цена не может
быть отрицательной»; «неизвестный профиль "x"»; «в команде нужна подстановка {prompt}»; «для CLI-провайдера
нужен профиль или команда»; «веб-чаты выключены»; «неизвестный веб-чат "x"»; «браузер должен быть chromium
или chrome»; «тип должен быть openai, anthropic, cli или web»), `409` (утилита не установлена — см. выше;
или «провайдер "x" уже есть» при `store.ErrExists`), `500`.

Побочные эффекты: `INSERT INTO providers` (`Store.AddProvider`). Колонка `caps` не пишется — у нового
провайдера теги определяются автоматически.

### 4.3. `DELETE /api/providers/{name}`

`deleteProvider`, `api.go:474`. Если провайдер типа `web` и веб-чаты включены — сначала
`Web.RemoveProfile(name)`: удаляется папка профиля браузера `<Home>/web-profiles/<name>` (cookies, вход).
Потом `Store.DeleteProvider` (ключи удаляются каскадом `ON DELETE CASCADE`).

Ответ `204`. Ошибки: `404 "провайдер не найден"`, `500` (ошибка удаления профиля или БД).

### 4.4. `POST /api/providers/{name}/keys` — добавить API-ключ

`addKey`, `api.go:493`. Тело: `{"label": string, "secret": string}`. `secret` обрезается, пустой → `400 "пустой ключ"`.
Ключ шифруется vault (AES-256-GCM) и сохраняется в `api_keys.secret` (`Store.AddKey`).

Ответ `201 {"id": int64}`. Ошибки: `400`, `404 "провайдер не найден"`, `500`.

Ключи провайдера OmniRoute перебирает по порядку `id` (ротация, см. 01-omniroute.md). Включить обратно
выключенный (`disabled=1`) ключ через API нельзя — только удалить и добавить заново.

### 4.5. `DELETE /api/keys/{id}`

`deleteKey`, `api.go:523`. `id` — целое. Ответ `204`. Ошибки: `400 "некорректный id"`, `404 "ключ не найден"`, `500`.

### 4.6. `GET /api/providers/{name}/remote-models` — модели с сервера провайдера

`remoteModels`, `api.go:202`. Берёт **первый активный** ключ провайдера (`Store.Keys` — только `disabled=0`,
по `id`); если ключей нет — пустая строка. Вызывает `ListModels(ctx, p, key)` с таймаутом 30 с
(для OpenAI-совместимых — `GET <base_url>/models` и т. п., см. 01-omniroute.md).

Ответ `200 {"models": string[]}`. Ошибки: `404`, `500`, `502 "не удалось получить список моделей: …"`.

Побочные эффекты: сетевой запрос к провайдеру с расшифрованным ключом.

### 4.7. `PUT /api/providers/{name}/models` — заменить список моделей

`setModels`, `api.go:230`. Тело `{"models": string[]}`. Пустые и содержащие `,` строки отбрасываются
(в БД модели хранятся строкой через запятую). Ответ `204`. Ошибки: `400`, `404`, `500`.

### 4.8. `GET /api/caps` — каталог возможностей

`capsCatalog`, `api.go:310`. Ответ — `omniroute.CapCatalog` (`internal/omniroute/caps.go:30`):
массив `{id, title, hint}`. Текущие `id`: `mcp`, `tools`, `vision`, `audio`, `voice`, `image_gen`, `web`,
`long_context`.

### 4.9. `PUT /api/providers/{name}/caps` — задать теги вручную

`setCaps`, `api.go:315`. Тело `{"caps": string[] | null}`. `null` (или отсутствие поля) — вернуть
автоопределение (в БД `caps = ''`). Каждый тег проверяется: если `omniroute.CapTitle(c) == c`, тег считается
неизвестным → `400 "неизвестная возможность <c>"`.

Ответ `200 {"caps": string[], "caps_auto": bool}` — действующие теги после изменения.
Ошибки: `400`, `404`, `500`. Тег `mcp` влияет на то, получает ли CLI-исполнитель MCP Edy
(`mcpBridge.Supports`, `mcpapi.go:222`); тег `vision` — может ли исполнитель видеть вложения
(`internal/app/app.go:123-127`).

---

## 5. Профили (presets) и установка утилит

### 5.1. `GET /api/presets`

`presets`, `api.go:173`. Ответ `200`:

```json
{
  "cli": [{"name": "...", "title": "...", "command": ["..."], "install": "...", "note": "..."}],
  "api": [{"name": "...", "title": "...", "kind": "openai|anthropic", "base_url": "...", "key_url": "...", "note": "..."}],
  "web": [{"name": "...", "title": "...", "url": "...", "min_interval_sec": 0, "daily_limit": 0}]
}
```

* `cli` — из `omniroute.CLIPresets`, `api` — из `omniroute.APIPresets` (`internal/omniroute/presets.go`),
  `web` — из `Web.Sites()` (пусто, если веб-чаты выключены). Все три списка отсортированы по `title`.
* Типы: `cliPresetJSON` `api.go:148`, `webPresetJSON` `api.go:156`, `apiPresetJSON` `api.go:164`.
* Ошибка `500` — если `Web.Sites()` вернул ошибку.

### 5.2. `POST /api/install` — установить CLI-утилиту профиля

`install`, `api.go:540`. Тело `{"preset": string}`. Команда установки берётся **только** из встроенного
профиля `omniroute.CLIPresets[preset].Install` — произвольную команду через API выполнить нельзя.

Выполняется `/bin/bash -c "<install>"` (`runInstall`, `api.go:672`) с таймаутом `installTimeout = 10 мин`
(`api.go:42`); запрос блокируется до окончания. После установки бинарник ищется через `FindExecutable`.

Ответ `200 {"path": "<абсолютный путь>", "output": "<вывод установки>"}`.
Ошибки: `400` (JSON, «неизвестный профиль»), `502 {"error":"установка не удалась: …","output":"…"}`,
`502 {"error":"установка завершилась, но утилита не найдена","output":"…"}`.

Побочные эффекты: запуск shell-команды на машине пользователя (например, `npm i -g …`, `brew install …`).

---

## 6. Расходы, бюджет, статистика моделей

### 6.1. `GET /api/usage`

`usage`, `api.go:683`. Считает с **локальной полуночи** текущего дня. Ответ `usageJSON` (`api.go:677`):

| Поле | Тип | Источник |
|---|---|---|
| `today_usd` | number | `Store.SpentSince(midnight)` — сумма `usage.cost_usd` |
| `budget_usd` | number | настройка `daily_budget_usd` (0, если не задана/не число) |
| `items` | `UsageSummary[]` | `Store.UsageSince(midnight)`: `{client, provider, model, requests, input_tokens, output_tokens, cost_usd}`, дорогие первыми |

`client` — значение заголовка `X-Edy-Client` у запроса в OmniRoute (`orchestrator`, `agent`, `reviewer`,
`embeddings`, `chat` из интерфейса, пусто/другое — внешние программы).

### 6.2. `PUT /api/budget`

`setBudget`, `api.go:702`. Тело `{"usd": number}`; отрицательное или невалидное → `400 "бюджет должен быть
неотрицательным числом"`. Сохраняется в `settings.daily_budget_usd` как `strconv.FormatFloat(v,'f',-1,64)`.
`0` означает «без лимита» (OmniRoute `checkBudget` игнорирует лимит ≤ 0). Ответ `204`.

### 6.3. `GET /api/stats/models?days=N`

`modelStats`, `projects.go:370`. `days` — 1…365, иначе 30. Ответ — `[]store.ModelStat`:
`{model:"провайдер/модель", attempts, successes, cost_usd, tokens}`, отсортировано по `attempts` убыв.,
затем по имени. Источник — `Store.ModelStats` (см. 06-store.md): попытки исполнителей из таблицы `agents`
(роль `executor`, не `running`), успехи — `state = done`; расходы и токены — из `usage` за тот же период.

---

## 7. Веб-чаты

Все маршруты требуют `h.Web != nil`, иначе `400 "веб-чаты выключены"` (кроме `status`/`browsers`, которые
возвращают пустые/ложные значения).

Вспомогательная `webConn` (`api.go:569`): читает тело `{"provider": string}`, находит провайдера типа `web`
(иначе `404 "веб-провайдер не найден"`) и собирает `webchat.Conn{Profile: name, Site, Browser}`.

Тип `webchat.Status` (`internal/webchat/manager.go:51`): `{state, message?, at?}`; `state` ∈
`unknown`, `login_open`, `checking`, `ok`, `login_required`, `error`.

### 7.1. `POST /api/web/login`

`webLogin`, `api.go:592`. Тело `{"provider": string}`. Открывает сайт в обычном (неуправляемом) окне
браузера профиля, чтобы пользователь вошёл в аккаунт. При первом входе с `chromium` скачивается изолированный
Chromium в `<Home>/chromium` — это может занять минуты (таймаут 10 мин). Ответ `200 Status` (обычно
`login_open`). Ошибки: `400`, `404`, `502` (ошибка браузера/скачивания).

### 7.2. `POST /api/web/check`

`webCheck`, `api.go:607`. Тело `{"provider": string}`. Запускает управляемый браузер и проверяет, что сайт
открывается и есть поле ввода. Ответ `200 Status` (`ok`, `login_required`, `error` …). Ошибки — только
`400`/`404`; ошибки проверки возвращаются внутри `Status`.

### 7.3. `GET /api/web/status`

`webStatus`, `api.go:618`. Ответ `200 {"<имя провайдера>": Status, …}` для всех провайдеров `web`.
`{}`, если веб-чаты выключены. `500` — ошибка БД.

### 7.4. `GET /api/web/browsers`

`webBrowsers`, `api.go:635`. Ответ `200 {"chromium": bool, "chrome": bool}` — найден ли браузер
(для `chromium` — скачан ли в папку данных).

---

## 8. Проекты

Проект — абсолютный путь к папке. Список хранится в настройке `projects` (JSON-массив строк,
`projects.go:27`), **не более 10**, новые/последние добавленные — первыми.
`projectPaths()` `projects.go:69`, `saveProjectPaths()` `projects.go:79`.

Тип ответа `projectJSON` (`projects.go:58-67`), его собирает `projectInfo` (`projects.go:84`):

| Поле | Тип | Значение |
|---|---|---|
| `path` | string | путь |
| `name` | string | `filepath.Base(path)` |
| `branch` | string, omitempty | текущая ветка (только если git) |
| `ready` | bool | `agent.CheckClean` прошёл: есть git, папка существует, в репозитории есть коммиты и нет незакоммиченных изменений (для не-git папки — `true`) |
| `problem` | string, omitempty | текст ошибки `CheckClean` |
| `running` | bool | в проекте идёт задача (`Tasks.Running`) |
| `pending` | bool | готовый результат ждёт Approve/Reject (`Tasks.Pending`) |
| `git` | bool | папка — git-репозиторий |

`projectInfo` запускает несколько `git`-команд, поэтому `GET /api/projects` на 10 проектах не мгновенный.

### 8.1. `GET /api/projects`

`listProjects`, `projects.go:102`. Ответ `200 projectJSON[]`.

### 8.2. `POST /api/projects` — добавить (или поднять наверх)

`addProject`, `projects.go:123`. Тело `{"path": string}`. `cleanProjectPath` (`projects.go:111`): путь должен
быть абсолютным (`400 "укажите полный путь к папке проекта, …"`), после `filepath.Clean` — существующей
папкой (`400 "папка не найдена"`). Путь ставится первым, остальные — следом без дубликатов, список
обрезается до 10 (**самый старый проект молча выпадает**). Ответ `200 projectJSON`.

Побочные эффекты: запись настройки `projects`. `CheckClean` может дописать `.edy/` в
`.git/info/exclude` проекта (`excludeEdyDir`, `internal/agent/task.go`).

### 8.3. `DELETE /api/projects?path=…`

`removeProject`, `projects.go:149`. Убирает путь из списка (`204`). Ничего больше не удаляется:
настройки `project:<path>`, задачи доски, память, команда, эмбеддинги остаются в БД (если проект добавить
заново — всё вернётся). Ошибка `500` — БД.

### 8.4. `GET /api/projects/checkpoints?path=…` — снимки Shadow Git

`checkpoints`, `projects.go:175`. Если нет `<path>/.edy/shadow.git` — `[]`. Иначе
`agent.Shadow{Root}.List` → массив `{id, message, at}` (`internal/agent/shadow.go:23`).
Ошибки: `404` (проект), `500`.

### 8.5. `POST /api/projects/restore` — откатить к снимку

`restore`, `projects.go:192`. Тело `{"path": string, "id": string}`. Если в проекте идёт задача —
`409 "в проекте идёт задача — сначала остановите её"`. Вызывает `Shadow.Restore(id)` — файлы проекта
приводятся к состоянию снимка. Ответ `204`; ошибка восстановления — `400` с текстом.

### 8.6. `GET /api/projects/context?path=…&q=…` — что видят агенты

`projectContext`, `projects.go:216`. Строит `codeindex.Build(path)` (скелет: go/ast, Python ast, BM25).
Без `q` — `{"text": idx.Skeleton(60000)}` (скелет проекта до 60 000 символов); с `q` —
`{"text": codeindex.FormatHits(idx.Search(q, 8), 40)}` (8 лучших кусков по BM25, до 40 строк каждый).
Ошибки: `404`, `500`.

### 8.7. `GET /api/projects/files?path=…`

`projectFiles`, `projects.go:912`. `agent.Workspace{Root}.Files(5000)` — относительные пути файлов
(без `.git`, `.edy`, `node_modules`, `vendor`, venv…). Ответ `{"files": string[], "truncated": bool}`
(`truncated` = файлов ≥ 5000).

### 8.8. `GET /api/projects/file?path=…&file=…`

`projectFile`, `projects.go:929`. `Workspace.ReadText(file, 1 МБ)` — только внутри проекта (выход за
пределы через `..`/симлинки запрещён в `Workspace`). Ответ `{"file": string, "text": string, "truncated": bool}`.
Ошибка чтения — `400`.

---

## 9. Чат с Оркестратором

### `POST /api/orchestrator`

`orchestrate`, `projects.go:233`. Если `h.LLM == nil` — `503 "Оркестратор недоступен"`.

Тело:

| Поле | Тип | Значение |
|---|---|---|
| `project` | string | путь проекта (проверяется `knownProject`) |
| `model` | string | модель Оркестратора `провайдер/модель` |
| `messages` | `agent.Msg[]` | история: `{role, content}`; учитываются только `user` и `assistant` |
| `conversation` | string | id разговора (для журнала агентов); 1–64 символа без пробелов и `/` |
| `executors` | string[] | выбранный исполнитель и резерв (`провайдер/модель`) |

Что происходит:
1. `journalOrchestrator` (`projects.go:404`): если `conversation` валиден и история не пуста — агент
   `orch-<conversation>` (роль `orchestrator`) создаётся при первой реплике (goal — первая реплика), иначе
   `TouchAgent(running)`; последняя реплика пользователя пишется событием `user`.
2. Собирается `orchestrator.Context` (`orchestratorContext`, `projects.go:305`):
   * `Executors` — описание выбранных моделей (теги, MCP, статистика за 30 дней — `statLine`, `projects.go:355`);
   * `Others` — до 30 других подключённых моделей;
   * `Skills` — общие и проектные скиллы (`skills.Load`);
   * `Servers` — включённые серверы каталога MCP (`orchestratorServers`, `projects.go:384`);
   * `Git` — проект git-репозиторий; `Team` — включённые агенты команды (`teamMembers`, `teamapi.go:140`);
   * `Parallel` — `ProjectSettings.MaxParallel`;
   * `Memory` — `memory.Brief(notes, последняя реплика, 2000)` — выжимка памяти проекта.
3. `orchestrator.Chat(ctx, LLM, model, project, history, oc)` с таймаутом 10 мин (запрос держится открытым).
4. Журнал: события `info` (передача агенту команды; выданные MCP/скиллы), `model` (текст ответа), `plan`;
   итог `TouchAgent(done, "Предложен план" | "Ждёт ответа разработчика")` или `failed`.

Ответ `200 orchestrator.Reply` (`internal/orchestrator/orchestrator.go:40`):

```json
{
  "text": "ответ без служебных блоков",
  "plan": "план (если составлен)",
  "mcp": ["имена серверов каталога"],
  "skills": ["имена скиллов"],
  "new_mcp": [{"name","description","type","url","command","args","secrets"}],
  "subtasks": [{"title","goal","model","agent","after":[1]}],
  "agent": "имя агента команды"
}
```

Ошибки: `400`, `404`, `502` (ошибка модели/OmniRoute, в т. ч. «дневной бюджет исчерпан»), `503`.

---

## 10. Доска задач

Все маршруты работают через `tasks.Manager` (см. 03-orchestrator-board.md). Тип ответа — `tasks.View`
(`internal/tasks/tasks.go:80-126`):

| Поле | Тип | Значение |
|---|---|---|
| `id` | string | `task_<hex>` |
| `project` | string | путь проекта |
| `parent` | string? | родитель (подзадача) |
| `kind` | string? | `epic` / `sub` / пусто |
| `agent` | string? | id агента команды |
| `step`, `after` | int?, int[]? | номер подзадачи и зависимости |
| `title`, `request` | string | название, исходная просьба |
| `issue_url`, `issue_ref` | string? | импорт из issue |
| `plan`, `goal` | string | утверждённый план, цель для исполнителя |
| `models` | string[] | исполнитель и резервы |
| `mcp`, `skills` | string[]? | выданные MCP-серверы и скиллы |
| `state` | string | `backlog`, `queued`, `running`, `done`, `needs_human`, `failed`, `cancelled`, `approved`, `rejected`, `merged` |
| `error`, `summary`, `model`, `diff` | string? | итог работы |
| `review` | `{model, verdict: ok|issues|error, text}`? | ревью |
| `branch`, `pr_url`, `note` | string? | итог Approve |
| `agents` | string[] | id агентов журнала (`<task>-1`, `<task>-2`, `<task>-review`…) |
| `attachments` | string[]? | имена файлов вложений |
| `position` | int64 | порядок в колонке |
| `created`, `updated` | time | |
| `events` | `BoardEvent[]` | `{id, at, kind, text}` после `from` (в `List` — пусто) |
| `next_from` | int64 | id последнего события, для следующего опроса |

Если `h.Tasks == nil`, большинство маршрутов отвечают `404 "задача не найдена"`, `GET /api/tasks` — `[]`,
`POST /api/tasks` — `503 "задачи недоступны"`.

### 10.1. `GET /api/tasks?project=…`

`listTasks`, `projects.go:421`. `project` необязателен (пусто — все проекты); если задан — `knownProject`.
Ответ `View[]` без событий.

### 10.2. `GET /api/tasks/{id}?from=N`

`getTask`, `projects.go:733`. Карточка с событиями, у которых `id > from` (интерфейс опрашивает с
`from = next_from`). `404`, если задачи нет.

### 10.3. `POST /api/tasks` — поставить в очередь с планом

`startTask`, `projects.go:451`. Тело:

| Поле | Тип | Значение |
|---|---|---|
| `id` | string | задача из пула (пусто — новая) |
| `project` | string | путь проекта |
| `parent` | string | родительская задача |
| `title`, `request` | string | название и просьба |
| `plan` | string | утверждённый план Оркестратора (пусто — «напрямую», без плана) |
| `comment` | string | пояснение Оркестратора (пишется комментарием от «Оркестратор») |
| `models` | string[] | исполнитель и резервы (`checkModels`) |
| `conversation` | string | id разговора (в журнал `orch-<id>` пишется «План утверждён…») |
| `subtasks` | `orchestrator.Subtask[]` | ≥ 2 — эпик на несколько агентов |
| `agent` | string | id агента команды, выбранного вручную |

Алгоритм:
1. `knownProject`, `checkModels(models)`.
2. При непустом плане: `opts.MCP = orchestrator.PlanMCP(plan, servers)`, `opts.Skills = PlanSkills(plan, skills)`
   (если скиллов нет — пустой срез, т. е. «никаких»; без плана `opts.Skills == nil` — «все скиллы»).
3. `id` задан → задача должна существовать и принадлежать проекту (`404`). `id` пуст и плана нет → проект
   должен быть «чистым» (`CheckClean`), если в нём ничего не выполняется (`400` с текстом проблемы).
4. Агент команды: `agent` (должен существовать и быть из этого проекта, иначе `400 "агент команды не найден"`)
   или, если подзадач < 2, — `orchestrator.PlanAgent(plan, team)` по имени. У агента берутся его модели
   (`agentModels`: модель + резерв), MCP и скиллы объединяются с выданными (`union`).
5. Если `id` задан, план непуст и подзадач < 2 — `Tasks.Unsplit(id)` (удаляет прежние подзадачи эпика;
   ошибка → `409`).
6. Подзадач ≥ 2 → `Tasks.EnqueueEpic`: для каждой подзадачи — агент команды по имени или модель
   `st.model` (+ резервная модель задачи `models[1]`, если она отличается). Иначе — `Tasks.Enqueue`.
7. Ответ `201 View` (`Tasks.Get(id, 0)`).

Ошибки: `400` (JSON, модель, агент, проект не готов, ошибки `Enqueue`), `404`, `409`, `503`.
Побочные эффекты: запись в `board_tasks`/`board_events`; если проект свободен — **сразу запускается
горутина исполнителя** (агенты в журнале, Shadow Git, хуки, расходы токенов).

### 10.4. `POST /api/board` — положить задачу в пул (без запуска)

`createTask`, `projects.go:581`. Тело `{"project","parent","title","request"}` (строки). Ответ `201 View`
(`state: backlog`). Ошибки: `400`, `404`.
> Если `h.Tasks == nil`, обработчик просто возвращается, не написав ответа (клиент получит пустой `200`).

### 10.5. `POST /api/board/issue` — импорт issue

`importIssue`, `projects.go:604`. Тело `{"project": string, "url": string}`.
1. `gitops.ParseIssueURL(url, accounts)` — GitHub/GitLab/Gitea (свой хост распознаётся по аккаунтам).
2. Аккаунт с тем же хостом (`TrimRight(host,"/") == ref.Host`) — его токен используется для запроса
   (приватные репозитории).
3. `gitops.FetchIssue` с таймаутом 30 с.
4. Создаётся задача в пуле: `title = "#<N> <title>"`, `request = "Issue: <url>\nМетки: …\n\n<title>\n\n<body>"`
   (тело обрезается до 6000 рун), `issue_url`, `issue_ref` (`owner/repo#N` для GitHub, `#N` для остальных —
   чтобы коммит закрыл issue).

Ответ `201 View`. Ошибки: `400` (JSON, ссылка), `404`, `502` (запрос к хостингу).
> Как и `createTask`, при `h.Tasks == nil` отвечает пустым `200`.

### 10.6. Шаблоны задач

Встроенные шаблоны — `builtinTemplates` (`projects.go:660`, 8 строк с подстановкой `{файл}`); свои —
настройка `task_templates` (JSON-массив строк).

* `GET /api/templates` — `templates`, `projects.go:680`. Ответ `{"builtin": string[], "custom": string[]}`.
* `POST /api/templates` — `addTemplate`, `projects.go:688`. Тело `{"text": string}`; пусто → `400 "пустой шаблон"`,
  > 1000 рун → `400`, уже есть → просто список, ≥ 50 своих → `400 "не больше 50 своих шаблонов"`.
  Ответ `200` — как у `GET`.
* `DELETE /api/templates?text=…` — `deleteTemplate`, `projects.go:720`. Удаляет точное совпадение, ответ — список.
  Ошибка записи настройки игнорируется.

### 10.7. `PUT /api/tasks/{id}` — переместить / отредактировать

`editTask`, `projects.go:747`. Тело `{"title"?: string, "request"?: string, "position"?: int64}` (все поля —
указатели: отсутствующее поле не меняется). `position` → `Tasks.Move`; `title`/`request` → `Tasks.Edit`
(только для задач в пуле, иначе «менять можно только задачи в пуле»). Если передано только одно поле,
второе уходит пустой строкой: пустой `request` не меняется, а пустой `title` пересчитывается из просьбы
(`titleOf`, `internal/tasks/tasks.go:873`) — т. е. правка одной просьбы сбрасывает ручное название. Ошибки → `409` с текстом. Ответ `200 View`.

### 10.8. `DELETE /api/tasks/{id}`

`deleteTask`, `projects.go:784`. `Tasks.Delete` — нельзя удалить работающую и ждущую решения (`409`).
Ответ `{"ok": true}`.

### 10.9. `POST /api/tasks/{id}/{action}`

`taskAction`, `projects.go:796`. `action`:

| action | Вызов | Эффект |
|---|---|---|
| `approve` | `Tasks.Approve` | оформление результата по настройкам проекта (`Finalizer`, `gitapi.go:290`): коммит в ветку `edy/…`/текущую/без коммита, push, PR/MR; запись итога в память; запуск следующей задачи очереди |
| `reject` | `Tasks.Reject` | откат проекта к состоянию до задачи (Shadow Git), урок в память, следующая задача |
| `cancel` | `Tasks.Cancel` | остановить работающую или снять с очереди |
| `comment` | `Tasks.Comment(id, "Вы", text)` | тело `{"text": string}` (ошибка JSON игнорируется); комментарий в историю |

Другой `action` → `404 "неизвестное действие"`. Ошибка операции → `409`. Ответ `200 View`.

### 10.10. `GET /api/tasks/{id}/patch`

`taskPatch`, `projects.go:878`. Ответ `{"patch": string}` — полный diff результата (хранится в записи задачи,
доступен и после Approve). Ошибка → `404`.

### 10.11. `POST /api/tasks/{id}/revert-file`

`revertFile`, `projects.go:891`. Тело `{"file": string}` (пусто → `400 "укажите файл"`). Откатывает один
файл результата к состоянию до задачи. Ошибка → `409`. Ответ `200 View`.

### 10.12. Вложения (скриншоты)

Хранятся в `<Home>/attachments/<task_id>/<N>-<имя>.<ext>` (`internal/tasks/attachments.go`).

* `POST /api/tasks/{id}/attachments` — `addAttachment`, `projects.go:830`. Тело
  `{"name": string, "data": "data:image/png;base64,…"}`; тело читается не более 8 МБ (иначе
  `400 "некорректный JSON или картинка больше 5 МБ"`). Допустимы PNG/JPEG/GIF/WebP, ≤ 5 МБ после
  декодирования, ≤ 8 вложений на задачу. Ответ `201 View`. Ошибки → `400`.
* `GET /api/tasks/{id}/attachments/{file}` — `getAttachment`, `projects.go:851`. Отдаёт файл через
  `http.ServeFile` с `Cache-Control: private, max-age=3600`. `404`, если нет.
* `DELETE /api/tasks/{id}/attachments/{file}` — `deleteAttachment`, `projects.go:865`. Ответ `200 View`, `404`.

Исполнитель получает вложения как картинки, только если у модели тег `vision`.

---

## 11. Git

### 11.1. Аккаунты хостингов

* `GET /api/git/accounts` — `gitAccounts`, `gitapi.go:34`. Ответ `GitAccount[]`:
  `{id:int64, kind:"github|gitlab|gitea", host, label, username}` — **токен не отдаётся** (`json:"-"`).
* `POST /api/git/accounts` — `addGitAccount`, `gitapi.go:43`. Тело
  `{"kind","host","label","username","token"}` (всё обрезается, у `host` убирается хвостовой `/`).
  Умолчания хоста: `github` → `https://github.com`, `gitlab` → `https://gitlab.com`, `gitea` — обязателен.
  Хост должен быть `http(s)://…`. Пустой токен → `400 "нужен токен доступа"`. Пустой `label` → `kind`.
  Токен шифруется vault. Ответ `201 {"id": int64}`. Ошибки: `400`, `500`.
* `DELETE /api/git/accounts/{id}` — `deleteGitAccount`, `gitapi.go:93`. Ответ `204`; `404 "аккаунт не найден"`.
  Ссылки на аккаунт в настройках проектов (`account_id`) **не** чистятся.

Вспомогательная `account(id)` (`gitapi.go:105`): `id == 0` → `nil` (без аккаунта), иначе аккаунт с
расшифрованным токеном или ошибка `аккаунт git: …`.

### 11.2. `POST /api/git/clone`

`gitClone`, `gitapi.go:117`. Тело `{"url": string, "account_id": int64}`.
* `ReposDir` пуст → `503 "клонирование недоступно"`.
* `gitops.RepoName(url)` — имя папки (`400` при ошибке).
* Папка назначения: `<Home>/repos/<name>`, при занятости — `<name>-2`, `<name>-3`…
* `gitops.Clone(ctx, url, dest, acc)` с таймаутом 10 мин (`502` при ошибке). Токен аккаунта передаётся git
  без записи в `.git/config` (см. gitops).
* Путь добавляется **первым** в список проектов (до 10). Если указан аккаунт — сохраняются настройки проекта
  `DefaultProjectSettings()` с `account_id`.

Ответ `200 projectJSON`.

### 11.3. `GET /api/git/info?path=…`

`gitInfo`, `gitapi.go:173`. Ответ `gitInfoJSON` (`gitapi.go:168`) — поля `gitops.Info` на верхнем уровне плюс
`settings`:

```json
{
  "is_repo": true, "branch": "main", "remote": "https://github.com/o/r.git (без логина/пароля)",
  "changed": ["строки git status --short"], "log": ["8 последних коммитов: %h %s (%cr)"],
  "settings": {"commit":"branch","push":"off","account_id":0,"author_name":"","author_email":"",
               "prefix":"Edy: ","auto_approve":false,"review_model":"","max_parallel":0}
}
```

### 11.4. `PUT /api/git/settings`

`gitSettings`, `gitapi.go:186`. Тело `{"path": string, "settings": ProjectSettings}`. Проверки:

| Условие | Ошибка 400 |
|---|---|
| `commit` ∉ {`branch`, `direct`, `none`} | «режим коммита: branch, direct или none» |
| `push` ∉ {`off`, `push`, `pr`} | «после коммита: off, push или pr» |
| `push = pr` и `commit ≠ branch` | «Pull Request возможен только при коммите в новую ветку» |
| `push = pr` и `account_id = 0` | «для Pull Request выберите аккаунт» |
| задано только одно из `author_name`/`author_email` | «для своего автора коммитов укажите и имя, и email» |
| `account_id` не найден | «аккаунт git: …» |
| `max_parallel` < 0 или > 6 | «одновременно — от 1 до 6 агентов» (0 разрешён и означает 2) |
| `review_model` не пусто, не `off` и не `провайдер/модель` существующего провайдера | «ревьюер: …» |

Сохраняется в настройку `project:<path>` (JSON). Ответ `204`.

### 11.5. `POST /api/git/{action}` — Pull / Push

`gitRemoteAction`, `gitapi.go:248`. Тело `{"path": string}`. `action`: `pull` (`gitops.Pull`, ответ — вывод
git) или `push` (`gitops.Push` текущей ветки в `origin`, ответ «Отправлено в origin»). Используется аккаунт из
настроек проекта. Таймаут 5 мин. Если идёт задача — `409 "в проекте идёт задача — дождитесь её"`.
Ответ `{"output": string}`. Ошибки: `400`, `404` (проект или «неизвестное действие»), `409`, `502`.

### 11.6. `Finalizer` (не маршрут)

`Finalizer(st)` (`gitapi.go:290`) — функция `tasks.FinalizeFunc`, которую `app.Start` ставит в
`Tasks.Finalize`. При Approve: читает `ProjectSettings`, аккаунт, берёт заголовок — первую строку цели,
свободное имя ветки `agent.FreeBranch(agent.BranchName(goal, now))` и вызывает `gitops.Finalize`
(коммит/push/PR). Сообщение коммита: `summary + "\n\nМодель: <model>"`. Возвращает `Outcome{Branch, PRURL, Note}`.

---

## 12. Журнал агентов

### 12.1. `GET /api/agents`

`agents`, `gitapi.go:317`. Последние 200 агентов (новые первыми). Ответ `store.Agent[]`:
`{id, role, model, project, task_id, goal, state, summary, error, created, finished?, events:int}`.
Роли: `orchestrator`, `executor`, `reviewer`, `mcp`. Состояния: `running`, `done`, `failed`, `cancelled`,
`interrupted`.

### 12.2. `GET /api/agents/{id}?after=N`

`agentDetail`, `gitapi.go:326`. Ответ `{"agent": Agent, "events": AgentEvent[]}`, события с `id > after`
(`{id, at, kind, text}`; виды: `user`, `model`, `plan`, `tool`, `checkpoint`, `hooks`, `breaker`, `info`,
`error`, `review` и события доски). `404 "агент не найден"`.

---

## 13. Скиллы, каталог MCP, память, команда

### 13.1. Скиллы

Скилл — папка `<имя>/SKILL.md` (формат Claude Code). Общие — `<Home>/skills`, проектные —
`<project>/.edy/skills` (`skills.Dirs`, `internal/skills/skills.go:46`). Тип `skills.Skill`:
`{name, description, body?, path, scope: "global"|"project"}`.

* `GET /api/skills?project=…` — `listSkills`, `mcpapi.go:292`. Без `project` — только общие. Ответ
  `skillsJSON` (`mcpapi.go:272`): `{"skills": Skill[], "global_dir": string, "project_dir"?: string}`.
* `POST /api/skills` — `saveSkill`, `mcpapi.go:310`. Тело `{"project","scope":"global|project","name","description","body"}`.
  Папка по `skillScopeDir` (`mcpapi.go:357`): `scope=project` требует `project` (`400 "не выбран проект"`)
  и `knownProject`; иной scope → `400 "scope: global или project"`. `skills.Save` пишет `SKILL.md`
  (валидация имени — в пакете skills, ошибки → `400`). Ответ `201 {"path": string}`.
* `DELETE /api/skills?scope=…&project=…&name=…` — `deleteSkill`, `mcpapi.go:334`. Находит скилл по имени в
  папке scope и удаляет его папку (`skills.Remove`). `404 "скилл \"x\" не найден"`. Ответ `{"ok": true}`.

### 13.2. Каталог внешних MCP-серверов

Каталог, из которого Оркестратор выбирает серверы для исполнителя (см. 04-mcp.md).

* `GET /api/mcp-servers` — `listMCPServers`, `mcpcatalog.go:75`. Ответ
  `{"servers": mcpServerJSON[], "presets": mcpPreset[]}`.
  `mcpServerJSON` (`mcpcatalog.go:47`): `{name, description, account?, enabled, type:"http|stdio", url?, command?, args?, headers?: string[], env?: string[]}` —
  у `headers` и `env` отдаются **только имена**, значения (там токены) наружу не уходят.
  `mcpPresets` (`mcpcatalog.go:30`): `playwright`, `context7`, `fetch` (stdio) и `github` (http) — шаблоны
  для формы `{name,title,description,type,url?,command?,args?,secret?,note?}`.
* `POST /api/mcp-servers` — `saveMCPServer`, `mcpcatalog.go:89`. Тело (всё строки):
  `name`, `description` (обязательно — `400 "опишите, для чего сервер…"`), `account`, `type` (`http`/`stdio`),
  `url`, `command`, `args` (по аргументу в строке), `headers` (`Имя: значение` в строке), `env`
  (`ИМЯ=значение` в строке). Разбор строк — `pairs` (`mcpcatalog.go:133`), ошибка формата → `400`.
  `Store.SaveMCPServer` валидирует (имя `^[a-z0-9][a-z0-9_-]{0,31}$`, не `edy`; для http — URL `http(s)://`;
  для stdio — команда) и делает **upsert по имени** с `enabled = true`; конфиг (URL, команда, аргументы,
  заголовки, env) шифруется целиком. Ответ `201 {"name": string}`.
  > Повторный POST с тем же именем полностью заменяет конфиг: если не передать заголовки/env заново,
  > сохранённые токены пропадут; выключенный сервер снова станет включённым.
* `PUT /api/mcp-servers/{name}/enabled` — `toggleMCPServer`, `mcpcatalog.go:151`. Тело `{"enabled": bool}`.
  Ответ `{"ok": true}`; `404 "сервер не найден"`.
* `DELETE /api/mcp-servers/{name}` — `deleteMCPServer`, `mcpcatalog.go:169`. Ответ `{"ok": true}`; `404`.

### 13.3. Память проекта

Короткие заметки (≤ 400 символов, `memory.MaxNote`). Тип `store.Note`:
`{id, project, kind: fact|lesson|task|note, text, source, pinned, agent?, created}`.

* `GET /api/memory?project=…&agent=…&q=…` — `listMemory`, `memoryapi.go:23`. Все заметки проекта (новые первыми);
  `agent` — только личная память этого агента команды; `q` — `memory.Search(notes, q, 30)` (поиск по словам).
  Ответ `{"notes": Note[], "pinned_brief": string}`; `pinned_brief` — `memory.Pinned(notes, 1500)`:
  закреплённое, которое Оркестратор видит всегда.
* `POST /api/memory` — `addMemory`, `memoryapi.go:56`. Тело `{"project","text","kind","pinned":bool,"agent"}`.
  Пустой текст → `400 "пустая заметка"`; > 400 рун → `400`. Неизвестный `kind` → `note`. `agent` должен быть
  агентом команды этого проекта (`400 "агент команды не найден"`). `source = "user"`. Ответ `201 {"id": int64}`.
* `PUT /api/memory/{id}` — `pinMemory`, `memoryapi.go:99`. Тело `{"project": string, "pinned": bool}`.
  `UPDATE … WHERE id AND project` → `404 "заметка не найдена"`. Ответ `{"ok": true}`.
* `DELETE /api/memory/{id}?project=…` — `deleteMemory`, `memoryapi.go:119`. Аналогично. Ответ `{"ok": true}`.

### 13.4. Команда проекта (постоянные агенты)

Тип `store.TeamAgent` (`internal/store/team.go:25`):
`{id:"agent_<hex>", project, name, emoji, role, scope: string[], model, fallback, skills: string[], mcp: string[], enabled, created}`.

* `GET /api/team?project=…` — `listTeam`, `teamapi.go:54`. Ответ `teamAgentJSON[]` (`teamapi.go:19`):
  поля `TeamAgent` + `done` (задачи `approved`/`merged`), `failed` (`needs_human`/`failed`/`rejected`) —
  считаются по доске (`teamStats`, `teamapi.go:33`), `notes` — число заметок личной памяти.
* `POST /api/team` — `saveTeamAgent`, `teamapi.go:79`. Тело — `TeamAgent`. Без `id` — новый
  (`agent_` + 12 hex); с `id` — агент должен существовать в том же проекте (`404 "агент не найден"`),
  `created` сохраняется. Имя обрезается и должно быть уникальным в команде без учёта регистра (`400`).
  `model`/`fallback` проверяются `checkModels` (`400`). Пустые элементы `scope` выкидываются.
  `TeamAgent.Validate` (id по regexp, имя 1–40, модель с `/`, роль ≤ 4000) → `400`. Ответ `201 TeamAgent`.
* `DELETE /api/team/{id}` — `deleteTeamAgent`, `teamapi.go:128`. Удаляет агента **и его личную память**
  (`DELETE FROM project_memory WHERE agent = id`). Проект не проверяется. Ответ `{"ok": true}`; `404`.

Вспомогательные: `teamMembers` (`teamapi.go:140`) — включённые агенты для инструкций Оркестратора со
строкой статистики; `agentByName` (`teamapi.go:158`) — включённый агент по имени (без учёта регистра);
`agentModels` (`teamapi.go:169`) — `[model, fallback]`; `union` (`teamapi.go:177`) — объединение без повторов.

---

## 14. Поиск по смыслу (эмбеддинги)

Если `h.Semantic == nil` — все маршруты `503 "поиск по смыслу недоступен"` (`semanticReady`, `semanticapi.go:20`).
В штатной сборке `Semantic` всегда создан, а «выключенность» означает пустую модель.

Тип `semantic.Status` (`internal/semantic/semantic.go:123`):
`{model, indexing:bool, done:int, total:int, embedded:int, error?:string, updated?:time}`.

* `GET /api/semantic?project=…` — `semanticStatus`, `semanticapi.go:29`. Без `project` — `{model}`; с проектом
  (`knownProject`) — статус индекса проекта.
* `PUT /api/semantic` — `setSemantic`, `semanticapi.go:44`. Тело `{"model": "провайдер/модель" | ""}`.
  `SetModel` пишет настройку `embed_model` и сбрасывает кэш векторов. Без `/` → `400`. Ответ `{model}`.
* `POST /api/semantic/index` — `semanticIndex`, `semanticapi.go:63`. Тело `{"project": string}`. Без модели →
  `400 "сначала выберите модель эмбеддингов"`. `Semantic.Start(project, project, 30 мин)` запускает
  **фоновую горутину** индексации (если ещё не идёт). Ответ **`202 Accepted`** со `Status`. Прогресс — опросом
  `GET /api/semantic`. Индексация вызывает `/v1/embeddings` OmniRoute (клиент `embeddings`, расходы учитываются)
  и пишет таблицу `embeddings`.
* `GET /api/semantic/search?project=…&q=…` — `semanticSearch`, `semanticapi.go:88`. Пустой `q` → `400`.
  Строит `codeindex`, гибридный поиск (BM25 + векторы, слияние RRF) — 10 результатов. Ответ
  `{"hits": codeindex.Hit[], "note": string}`; `Hit = {path, symbol, start, end, score, text}`; `note` — причина,
  если работал только BM25.

---

## 15. Прочее

### 15.1. `GET /api/info`

`info`, `api.go:124`. Ответ:

```json
{"base_url": "http://<addr>/v1", "token": "<токен>", "mcp_url": "http://<addr>/mcp", "edy_path": "<путь к edy>"}
```

`edy_path` — `edyPath()` (`api.go:131`): путь к консольной команде для настройки MCP по stdio. Внутри
`Edy.app` окно — `Contents/MacOS/Edy`, а CLI — `Contents/Resources/bin/edy`; если её нет — путь текущего
исполняемого файла. Интерфейс показывает эти данные на экране «Подключение» и в сниппетах для Claude Code.

### 15.2. `GET /api/prefs`, `PUT /api/prefs`

`prefs` `api.go:276`, `setPrefs` `api.go:281`. Ответ `{"notify": bool}` (`true`, если настройка `notify_off != "1"`).
`PUT` тело `{"notify": bool}` (необязательное поле): `false` → `notify_off = "1"`, `true` → `"0"`. Ответ — как у GET.

### 15.3. `POST /api/notify/test`

`testNotify`, `api.go:302`. Показывает системное уведомление «Уведомления работают…». Ответ `{"ok": true}`;
`502 "macOS не показал уведомление: …"`.

### 15.4. `POST /api/open`

`openURL`, `api.go:644`. Тело `{"url": string}` — только `http`/`https` с хостом (`400 "можно открывать только
ссылки http(s)"`). Открывает в браузере по умолчанию (`open`/`xdg-open`). Нужно окну Wails: WKWebView не
открывает `target="_blank"` сам. Ответ `204`; `500` — ошибка запуска.

---

## 16. MCP по HTTP: `/mcp`

`mcpapi.go`. Обработчик возвращает `h.MCP()` (`mcpapi.go:48`) → `serveMCP` (`mcpapi.go:52`). Транспорт —
упрощённый MCP Streamable HTTP: JSON-RPC в теле POST, ответ — один JSON (без SSE). Протокол и инструменты —
`internal/mcp`, см. 04-mcp.md.

Аутентификация — Bearer-токен, как у `/api` (401 иначе).

| Метод | Поведение |
|---|---|
| `POST` | обработка JSON-RPC |
| `DELETE` | закрыть сессию из заголовка `Mcp-Session-Id`: удалить из памяти, агент → `done` «Клиент отключился»; ответ `204` |
| прочие (в т. ч. `GET`) | `405`, `Allow: POST, DELETE` (поток уведомлений сервера не поддерживается) |

Параметры запроса:
* `project=<путь>` — **обязателен** (`400 "укажите проект: /mcp?project=<путь>"`), проверяется `knownProject`.
* `bind=<ключ>` — привязка к задаче (выдаётся исполнителю Edy). Неизвестный ключ или чужой проект →
  `403 "привязка MCP к задаче устарела"`.

Тело ограничено 8 МБ. Из тела разбирается `method` и `params.clientInfo.{name,version}`.

Сессии:
* `method == "initialize"` → `newMCPSession` (`mcpapi.go:178`): id `mcp-<16 hex>`, возвращается в заголовке
  `Mcp-Session-Id`. Без привязки создаётся агент журнала с этим же id, роль `mcp`, модель = «имя версия»
  клиента (или «MCP-клиент»), цель «Внешний агент работает с проектом через MCP». В памяти держится не
  более `maxMCPSessions = 64` сессий; старейшие по `used` вытесняются (их агенты → `done` «Сессия закрыта»).
* Иначе, если есть `Mcp-Session-Id` — сессия ищется в памяти; не найдена или другой проект →
  `404 "сессия MCP не найдена — переподключитесь"` (после перезапуска Edy клиент должен заново сделать `initialize`).
* Без заголовка — одноразовый сервер без журнала.

Ответ: `mcp.Server.Handle(body)`; если `nil` (уведомление JSON-RPC) — `202 Accepted` без тела, иначе
`200` + `application/json`.

Настройка `mcp.Server` — `mcpServer` (`mcpapi.go:143`):
* **привязка к задаче** (`bind`): события и доступ берутся из `agent.MCPEvents` задачи (`OnCall`, `OnChange`,
  `Skills`, `Memory`, `Code`); правки не блокируются «занятостью» проекта (их делает сама задача).
* **внешний агент** (`agentID != ""`): память — `memory.Access{Source:"mcp:<id>", Limit:20}` (до 20 заметок за
  сессию), при наличии `Semantic` — ранжирование по смыслу и `search_code` по смыслу; каждый вызов
  инструмента пишется событием `tool`/`error`, снимок — `checkpoint`.
* `Busy` — если в проекте идёт задача Edy, правки внешнего агента блокируются.

`MCPBridge()` (`mcpapi.go:218`) — мост для исполнителей задач (`tasks.Manager.MCP`):
* `Supports(model)` — провайдер `cli` с тегом `mcp`.
* `Bind(root, ev)` — создаёт ключ `bind` (32 hex) и URL `http://<addr>/mcp?project=<root>&bind=<key>`;
  функция-очистка удаляет привязку и все **безжурнальные** сессии этого `root`.

`CloseMCP()` (`mcpapi.go:255`) вызывается из `app.Server.Close`: все сессии → агенты `done` «Edy закрыт».

---

## 17. Сводка побочных эффектов

| Эффект | Маршруты |
|---|---|
| Запись в `settings` | `PUT /api/budget`, `PUT /api/prefs`, `POST/DELETE /api/projects`, `POST /api/git/clone`, `PUT /api/git/settings`, `POST/DELETE /api/templates`, `PUT /api/semantic` |
| Шифрование секрета vault | `POST /api/providers/{name}/keys`, `POST /api/git/accounts`, `POST /api/mcp-servers` |
| Фоновые горутины | `POST /api/tasks` (исполнитель), `POST /api/tasks/{id}/approve|reject` (следующая в очереди), `POST /api/semantic/index` |
| Запуск внешних процессов | `POST /api/install` (bash), `POST /api/open` (open), `/api/web/*` (браузер), git: проекты, `checkpoints`, `restore`, `git/*`, `approve`/`reject` |
| Сетевые запросы наружу | `remote-models`, `board/issue`, `git/clone|pull|push`, `approve` (push/PR), `orchestrator` и задачи (через OmniRoute к провайдерам) |
| Изменение файлов проекта | `POST /api/projects/restore`, задачи (`approve`, `reject`, `revert-file`), MCP-инструменты |
| Файлы в папке данных | `attachments/`, `repos/`, `skills/`, `web-profiles/`, `chromium/` |
| Журнал агентов | `POST /api/orchestrator`, `POST /api/tasks`, `/mcp` |

---

## 18. Тесты

Тесты API лежат рядом: `api_test.go`, `board_test.go`, `gitapi_test.go`, `mcpapi_test.go`,
`mcpcatalog_test.go`, `memoryapi_test.go`, `projects_test.go`, `semanticapi_test.go`, `teamapi_test.go`.
Они создают `api.New` с временной базой и подменяют `FindExecutable`, `Install`, `ListModels`, `OpenURL`,
`Notify`. При добавлении маршрута: зарегистрируйте его в соответствующем `register*Routes`, проверьте проект
через `knownProject`, используйте `reply`/`fail`, добавьте тест и строку в таблицу раздела 3 этого документа.

## 19. Известные странности и долги

1. `createTask` и `importIssue` (`projects.go:592`, `613`) при `h.Tasks == nil` выходят, не записав ответ
   (клиент получит пустой `200`).
2. `gitSettings` разрешает `max_parallel = 0` (это «по умолчанию 2»), а текст ошибки говорит «от 1 до 6».
3. `DELETE /api/git/accounts/{id}` не чистит `account_id` в настройках проектов — следующий Approve с push/PR
   упадёт «аккаунт git из настроек проекта: store: не найдено».
4. `DELETE /api/projects` не удаляет связанные данные (настройки, доска, память, команда, эмбеддинги);
   список проектов ограничен 10 — одиннадцатый вытесняет самый старый молча.
5. `POST /api/mcp-servers` — upsert без слияния: значения заголовков/env, которые интерфейс не видит,
   при повторном сохранении теряются; `enabled` снова `true`.
6. `DELETE /api/team/{id}`, `PUT/DELETE /api/memory/{id}` не вызывают `knownProject`.
7. `taskAction` для `comment` игнорирует ошибку разбора JSON; `deleteTemplate` игнорирует ошибку записи.
8. Нет маршрута, чтобы снова включить выключенный OmniRoute ключ (`api_keys.disabled`).
9. Сессии MCP только в памяти: после перезапуска клиенты получают `404` и должны переподключиться.
