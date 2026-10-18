# 08. Сборка приложения, точки входа, сборка и релизы, окружение разработчика

Этот документ описывает, как Edy собирается в один процесс, какие у него точки входа (`cmd/edy`,
`cmd/edy-app`), где лежат данные, как собирать и тестировать проект и как готовить сборки для Mac.
Подробности отдельных подсистем вынесены в соседние документы `docs/dev/*`: MCP, UI, задачи и т. д.
Здесь они описаны ровно настолько, насколько нужно, чтобы понять сборку.

Все ссылки вида `файл:строка` даны по состоянию ветки `claude/elegant-edison-gu1b8f`
на коммит `b88fcbf` («Поиск по смыслу: эмбеддинги, гибридный search_code и память»).

---

## 1. Коротко

| Что | Где | Комментарий |
|---|---|---|
| Модуль | `github.com/laurenwilcox/name` | `go.mod:1`, Go `1.26.0` (`go.mod:3`) |
| CLI и сервер | `cmd/edy` | бинарник `edy`, собирается с `CGO_ENABLED=0` |
| Окно приложения | `cmd/edy-app` | Wails v2, собирается **только на Mac**, нужен CGO, тег `desktop` |
| Сборка процесса | `internal/app/app.go` | `Start()` связывает все компоненты; общая для `edy serve/ui` и окна |
| Сборка Edy.app | `scripts/build-macos-app.sh` | `build/Edy.app`, minimum macOS 13.0 |
| Адрес по умолчанию | `127.0.0.1:11435` | `omniroute.DefaultAddr`, `internal/omniroute/server.go:25` |
| Данные | `$EDY_HOME` или `os.UserConfigDir()/Edy` | `internal/app/app.go:34` |
| Мастер-ключ | системная связка ключей (macOS Keychain) | `internal/vault/vault.go:46`, сервис `dev.edy.ide`, пользователь `master-key` |

Edy — это **один процесс** с одним HTTP-сервером на одном порту. На нём одновременно работают
OpenAI-совместимый прокси OmniRoute (`/v1/…`), JSON API интерфейса (`/api/…`), MCP-сервер по HTTP (`/mcp`)
и сама страница интерфейса (`/`). Агенты внутри процесса ходят в OmniRoute **не по сети**, а через
in-process `http.Client` (`agent.InProcess`, `internal/agent/llm.go:170`).

---

## 2. Схема работающего процесса

```
                         ┌──────────────────────────────────────────────────────────────────┐
  Браузер (edy ui)  ───► │  net.Listen("tcp", addr)   addr = 127.0.0.1:11435 по умолчанию   │
  Окно Wails (Edy.app)──►│  http.Server{ReadHeaderTimeout: 10s}  (internal/app/app.go:173)  │
  curl / IDE / SDK  ───► │                                                                  │
  edy mcp (мост)   ───►  │  http.ServeMux (app.go:166-171)                                  │
                         │   ├─ "/v1/"     → omniroute.Server  (Bearer-токен)               │
                         │   │              GET /v1/models, POST /v1/chat/completions,      │
                         │   │              POST /v1/embeddings                             │
                         │   ├─ "/healthz" → omniroute.Server  ("ok", БЕЗ токена)            │
                         │   ├─ "/api/"    → api.Handler        (Bearer-токен)              │
                         │   ├─ "/mcp"     → api.Handler.MCP()  (MCP Streamable HTTP)       │
                         │   └─ "/"        → ui.Handler()       (встроенная страница)       │
                         └──────────────────────────────────────────────────────────────────┘
                                         │                        ▲
              ┌──────────────────────────┘                        │ in-process HTTP
              ▼                                                   │ (agent.InProcess(router),
   ┌──────────────────────── api.Handler (h) ─────────────────┐   │  BaseURL http://edy.local/v1)
   │ h.Web      = webchat.Manager (браузеры веб-чатов, CDP)   │   │
   │ h.LLM      = OmniRoute-клиент  X-Edy-Client: orchestrator├───┤
   │ h.Semantic = semantic.Engine   X-Edy-Client: embeddings  ├───┤
   │ h.Tasks    = tasks.Manager                               │   │
   │   ├─ LLM     OmniRoute-клиент  X-Edy-Client: agent       ├───┤
   │   ├─ Review  review.Reviewer   X-Edy-Client: reviewer    ├───┘
   │   ├─ Board/Journal/Notes/Team = *store.Store (SQLite)    │
   │   ├─ Semantic = h.Semantic                               │
   │   ├─ MCP      = h.MCPBridge()  (MCP Edy для исполнителей)│
   │   ├─ Notify   → h.Notify → notify.System (osascript)     │
   │   ├─ Finalize = api.Finalizer(st) (ветка/коммит/push/PR) │
   │   ├─ MaxParallel/AutoApprove/CanSee — из настроек        │
   │   └─ AttachDir = $HOME_EDY/attachments                   │
   │ h.ReposDir = $HOME_EDY/repos,  h.Home = $HOME_EDY        │
   └──────────────────────────────────────────────────────────┘
              │                                   │
              ▼                                   ▼
   omniroute.Server (router)               *store.Store  ← vault.Vault (AES-256-GCM)
     адаптеры: openai, anthropic, cli (PTY),     edy.db (modernc.org/sqlite, WAL)
     web → omniroute.WebAdapter{Browser: web}    мастер-ключ: Keychain (go-keyring)
```

Фоновые «менеджеры» внутри процесса:

- **`tasks.Manager`** — доска задач, очередь, эпики, агенты-исполнители (горутины). При старте вызывается
  `Recover()` (`internal/app/app.go:164`): задачи в состоянии `running` откатываются и помечаются ошибкой,
  очереди продолжаются (`internal/tasks/tasks.go:928`).
- **`webchat.Manager`** — процессы Chromium/Chrome для веб-чатов. Закрывается в `Server.Close()`.
- **MCP-сессии** `api.Handler` — закрываются `h.CloseMCP()` в `Server.Close()`.
- **`semantic.Engine`** — индексация эмбеддингов (по запросу из API и перед задачами доски).

Внешние программы (Claude Code, Gemini CLI, Cursor и т. д.) подключаются двумя способами:
1. по HTTP напрямую на `http://127.0.0.1:11435/mcp` с Bearer-токеном;
2. через `edy mcp` (stdio). Если Edy уже запущен, это мост в `/mcp`, иначе автономный сервер (см. §4.9).

### 2.1. Как `Start()` собирает компоненты (по шагам)

`internal/app/app.go:94` — `Start(st *store.Store, home, addr string, log *slog.Logger) (*Server, error)`:

1. `log == nil` → логгер в `io.Discard` (`:95-97`). Тесты передают `nil`.
2. `EnsureToken(st, home)` (`:98`) — токен OmniRoute из настройки `router_token`, копия в `router.token`.
3. `net.Listen("tcp", addr)` (`:102`). Если порт занят, ошибка «не удалось занять … (уже запущен другой Edy?)».
   Порт занимается **до** создания компонентов, поэтому второй экземпляр падает сразу. Адрес `127.0.0.1:0`
   (тесты) даёт случайный порт; реальный адрес — `ln.Addr()`.
4. `webchat.NewManager(home)`, лог в stderr (`:107-108`).
5. `omniroute.NewServer(st, tok, log)` + `RegisterAdapter(store.KindWeb, &omniroute.WebAdapter{Browser: web})`
   (`:110-111`). Адаптеры openai/anthropic/cli регистрируются внутри `NewServer`, web — только здесь, потому
   что ему нужен менеджер браузеров.
6. `api.New(st, tok, ln.Addr().String())` (`:112`). Адрес нужен экрану «Подключение» (`/api/info`).
7. In-process клиенты OmniRoute. Все они используют `BaseURL: "http://edy.local/v1"`: хост фиктивный,
   запрос уходит прямо в `router.ServeHTTP`. Метка `Client` уходит в заголовок `X-Edy-Client`
   (`internal/omniroute/server.go:32`, `internal/agent/llm.go:112`), по ней считаются расходы:
   - `h.LLM` — `Client: "orchestrator"` (`:114`), чат Оркестратора;
   - `tasks.NewManager(… Client: "agent" …, st)` (`:115`), исполнители задач;
   - `semantic.New(&semantic.Client{… Client: "embeddings"}, st)` (`:119`), эмбеддинги;
   - `review.New(… Client: "reviewer" …)` (`:129`), ревьюер перед Approve.
8. Поля `tasks.Manager` (определение — `internal/tasks/tasks.go:152-191`):
   | Поле | Значение в `Start` | Строка |
   |---|---|---|
   | `Journal` | `st` (журнал агентов) | `:116` |
   | `SkillsHome` | `home` (общие скиллы в `home/skills`) | `:117` |
   | `Notes` | `st` (память проекта) | `:118` |
   | `Semantic` | `h.Semantic` | `:120` |
   | `Team` | `st` (постоянные агенты) | `:121` |
   | `AttachDir` | `home/attachments` | `:122` |
   | `CanSee` | у провайдера модели есть тег `CapVision` | `:123-127` |
   | `MCP` | `h.MCPBridge()` | `:128` |
   | `Review` | `review.New(...)`; модель ревью из настроек проекта: `"off"` — без ревью, `""` — `review.AutoModel(v)`, иначе заданная | `:129-139` |
   | `Notify` | если настройка `notify_off` ≠ `"1"`, то `h.Notify("Edy · <папка>", title+"\n"+msg)` | `:140-147` |
   | `MaxParallel` | `ProjectSettings(project).MaxParallel` (0 → 2 внутри tasks) | `:148-151` |
   | `AutoApprove` | `ProjectSettings(project).AutoApprove` | `:152-155` |
   | `Finalize` | `api.Finalizer(st)` (`internal/api/gitapi.go:290`) | `:156` |
   `Board` (= `st`) и `LLM` задаются в `NewManager` (`internal/tasks/tasks.go:193`).
9. `h.ReposDir = home/repos`, `h.Home = home` (`:157-158`).
10. `st.InterruptRunningAgents()` (`:159`): агенты прошлого запуска в состоянии «работает» → «прерван».
11. `h.Tasks.Recover()` (`:164`).
12. Маршруты (`:166-171`) и `http.Server` в горутине (`:173-182`). Ошибка `Serve` (кроме `ErrServerClosed`)
    попадает в канал `done`, после чего канал закрывается.
13. Лог «OmniRoute запущен» (`:183`).

`Server.Close()` (`:188`): `Shutdown` с таймаутом 5 с, `api.CloseMCP()`, `web.Close()`.
**Задачи агентов при этом не останавливаются и не ждутся** (`tasks.Manager.Wait()` существует,
`internal/tasks/tasks.go:963`, но не вызывается). Процесс просто завершается, а при следующем старте
`Recover()` откатывает оборванные задачи.

---

## 3. Папка данных

`app.Home()` (`internal/app/app.go:34`):
- если задана переменная окружения `EDY_HOME`, используется она;
- иначе `filepath.Join(os.UserConfigDir(), "Edy")`:
  - macOS: `~/Library/Application Support/Edy`;
  - Linux: `$XDG_CONFIG_HOME/Edy`, обычно `~/.config/Edy`. Текст `usage` в `cmd/edy/main.go:54` и README
    упоминают только путь macOS;
- папка создаётся с правами `0700`.

Содержимое (кто создаёт):

| Путь | Что | Кто пишет |
|---|---|---|
| `edy.db`, `edy.db-wal`, `edy.db-shm` | SQLite: провайдеры, ключи (зашифрованы), настройки, расходы, агенты, доска, память, эмбеддинги, MCP-серверы, команда | `internal/app/app.go:56`, пакет `internal/store` |
| `router.token` | копия токена OmniRoute, права `0600`, с переводом строки | `internal/app/app.go:74` |
| `attachments/` | скриншоты и вложения задач | `internal/tasks/attachments.go` |
| `repos/` | клоны репозиториев из вкладки «Git» | `internal/api/gitapi.go:140` |
| `skills/<имя>/SKILL.md` | глобальные скиллы | `internal/skills/skills.go:49` |
| `chromium/` | скачанный изолированный Chromium (`edy web chromium`) | `internal/webchat/chrome.go:44,115` |
| `web-profiles/<провайдер>/` | профили браузера веб-чатов (cookies, вход) | `internal/webchat/manager.go:161` |
| `web-debug/` | скриншоты и JSON-диагностика сбоев веб-чатов | `internal/webchat/manager.go:825` |
| `web-sites.json` | необязательные переопределения селекторов сайтов | `internal/webchat/sites.go:77,86` |

В папках проектов (не в папке данных) Edy создаёт `.edy/` (Shadow Git `.edy/shadow.git`, скиллы проекта
`.edy/skills/`, хуки `.edy/hooks.json`) и прячет его через `.git/info/exclude`.

Мастер-ключ шифрования в папке **не лежит**: он хранится в системной связке ключей (см. §8).
Если удалить `edy.db`, пропадут все данные. Если удалить запись в Keychain, зашифрованные ключи
в `edy.db` больше не расшифровать: при следующем старте создастся новый мастер-ключ.

Для разработки и экспериментов удобно: `EDY_HOME=/tmp/edy-dev edy serve -addr 127.0.0.1:18080`.

---

## 4. Команда `edy` (`cmd/edy`)

Точка входа: `main()` (`cmd/edy/main.go:57`) → `run(args)` (`:64`). Разбор команд — `switch` по первым одному-двум
словам (`:75-124`). Подкоманды разбирают свои флаги через `flag.NewFlagSet(..., ContinueOnError)`.

**Важно.** До разбора команды всегда вызывается `openStore()` (`:69`), то есть `app.OpenStore()`:
открывается база и читается мастер-ключ из Keychain. Значит, **любая** команда, кроме вызова без аргументов
(печать справки, `:65-68`), требует доступной связки ключей. Это относится и к `edy mcp`, которой база не нужна.
Неизвестная команда печатает справку и завершается с ошибкой (`:121-123`). Ошибки печатаются как
`ошибка: …` в stderr с кодом выхода 1 (`:58-61`).

Команд `run` и `agent` **нет**. Агентная задача из терминала — `edy task`. Нет и команды `version`.

### 4.1. `edy` (без аргументов)
Печатает справку `usage` (`cmd/edy/main.go:33-55`).

### 4.2. `edy serve [-addr ADDR]`
`serve()` (`:131`). Флаг `-addr` (по умолчанию `omniroute.DefaultAddr` = `127.0.0.1:11435`).
Поднимает весь Edy через `app.Start`, пишет slog-лог в stderr и печатает `Интерфейс: http://…/#token=…`.
Работает до SIGINT/SIGTERM (`signal.NotifyContext`) или до самостоятельной остановки сервера (`srv.Done()`).
```sh
edy serve
edy serve -addr 127.0.0.1:18080
EDY_HOME=/tmp/edy-dev edy serve -addr 127.0.0.1:0      # случайный порт; реальный — в строке «Интерфейс:»
```
В логе «OmniRoute запущен» показан **запрошенный** адрес, а не фактический (`app.go:183` использует `addr`,
а не `ln.Addr()`). При `:0` правильный адрес есть только в строке «Интерфейс:».

### 4.3. `edy ui [-addr ADDR]`
То же, что `serve`, но после старта выполняет `open <URL>` (`:145-149`). `open` есть только в macOS.
На Linux команда выдаст предупреждение «не удалось открыть браузер», и адрес нужно открыть вручную
(API для внешних ссылок, `internal/api/api.go:664`, использует `xdg-open`, а `edy ui` — нет).

### 4.4. `edy token`
Печатает токен OmniRoute (создаёт при первом запуске) и перезаписывает `router.token` (`:80-85`).
```sh
curl -H "Authorization: Bearer $(edy token)" http://127.0.0.1:11435/v1/models
```
Формат токена: `edy-` + 48 hex-символов (`internal/omniroute/server.go:77`).

### 4.5. `edy provider add …`
`providerAdd()` (`:160`). Флаги:

| Флаг | Для | Смысл |
|---|---|---|
| `-name` | все | имя провайдера, префикс модели (`имя/модель`). Обязателен, без `/` |
| `-kind` | все | `openai` \| `anthropic` \| `cli` \| `web` (в тексте флага `web` не указан, но поддерживается) |
| `-base-url` | openai/anthropic | базовый URL API, например `http://localhost:11434/v1` |
| `-models` | все | модели через запятую; для `web` по умолчанию `chat` |
| `-price-in`, `-price-out` | все | USD за 1M входных/выходных токенов |
| `-preset` | cli | готовый профиль: `aider`, `amp`, `antigravity`, `claude-code`, `codex`, `copilot`, `cursor`, `gemini`, `goose`, `llm`, `ollama`, `opencode`, `qwen-code` (`internal/omniroute/presets.go:17`) |
| `-command` | cli | команда JSON-массивом с подстановками `{model}` и `{prompt}` |
| `-y` | cli | ставить утилиту профиля без вопроса |
| `-site` | web | `chatgpt` \| `claude` \| `gemini` |
| `-browser` | web | `chromium` (по умолчанию) \| `chrome` |

Для `-preset` вызывается `ensureInstalled()` (`:231`): ищет утилиту (`omniroute.FindExecutable`; кроме `PATH` смотрит
`~/.local/bin`, `/opt/homebrew/bin`, `/usr/local/bin` — `internal/omniroute/cli.go:20`). Если её нет, показывает команду установки и спрашивает `[y/N]` (принимаются `y`, `yes`,
`д`, `да`), запускает установку через `/bin/bash -c` (`runShell`, `:255`) и сохраняет **абсолютный путь**
к утилите (`:198`). `-preset` и `-command` вместе указывать нельзя.
```sh
edy provider add -name anthropic -kind anthropic -models claude-opus-5-5,claude-haiku-5-5 -price-in 4 -price-out 20
edy provider add -name ollama -kind openai -base-url http://localhost:11434/v1 -models llama3.1
edy provider add -name cc -kind cli -preset claude-code -models sonnet -y
edy provider add -name mycli -kind cli -command '["ollama","run","{model}","{prompt}"]' -models llama3.1
edy provider add -name gpt-web -kind web -site chatgpt -browser chromium
```

### 4.6. `edy provider list` / `edy provider remove -name N`
`providerList()` (`:403`) печатает таблицу: имя, тип, число ключей, URL или команда, модели.
`providerRemove()` (`:383`) у web-провайдера удаляет и папку профиля браузера (`RemoveProfile`),
затем провайдера вместе с ключами.

### 4.7. `edy key add -provider N [-label L]`
`keyAdd()` (`:424`). Ключ читается **из stdin** (одна строка), шифруется vault-ом и сохраняется.
```sh
edy key add -provider anthropic -label main          # вставить ключ и нажать Enter
printf '%s\n' "$KEY" | edy key add -provider openai  # из переменной, без истории shell
```

### 4.8. `edy web chromium` / `edy web login <провайдер>`
- `web chromium` (`:94-101`) скачивает изолированный Chromium в `home/chromium` и печатает путь.
- `web login <имя>` (`webLogin`, `:273`) открывает сайт веб-провайдера в обычном окне браузера.
  Пользователь входит и закрывает окно, после чего Edy проверяет подключение (опрос статуса каждые 500 мс).
  Пока идёт вход, `edy serve` для этого профиля лучше не использовать.

### 4.9. `edy mcp [-project DIR] [-addr ADDR]` (`cmd/edy/mcp.go`)
`runMCP()` (`cmd/edy/mcp.go:28`). MCP-сервер Edy на stdio для Claude Code, Gemini CLI, Cursor и других клиентов.
Флаги: `-project` (по умолчанию `.`), `-addr` (по умолчанию строка `"127.0.0.1:11435"`; константа
`DefaultAddr` здесь не используется, значение продублировано).
- Если по `-addr` отвечает `/healthz` (`app.Running`), команда читает `home/router.token`, регистрирует
  проект через `POST /api/projects` (`bridge.register`, `:70`) и работает **мостом**: каждая строка JSON-RPC
  из stdin уходит `POST /mcp?project=…` с заголовком `Mcp-Session-Id` (`bridge.forward`, `:112`). Ответ 202
  ничего не пишет, ответ 200 пишется в stdout строкой. Ошибка HTTP превращается в JSON-RPC ошибку `-32603`
  (`rpcFailure`, `:159`). По EOF сессия закрывается `DELETE /mcp` (`bridge.close`, `:146`). HTTP-таймаут 15 минут.
- Иначе работает автономный `mcp.Server{Root, Home}.ServeStdio` (`:57-58`).
- Подробности инструментов и протокола — в документе про MCP.
```sh
claude mcp add edy -- edy mcp -project "$PWD"
```

### 4.10. `edy task -model M[,M2…] [-project DIR] "задача"`
`runTask()` (`:302`). Это **старый терминальный путь** без доски задач: создаёт OmniRoute в процессе
(лог отключён), web-адаптер и `agent.Task` с клиентом `X-Edy-Client: agent`, грузит скиллы
(`skills.Dirs(home, root)`) и печатает события с иконками. Цепочка моделей идёт от слабой к сильной:
после 3 провалов задача переходит к следующей модели (Circuit Breaker). После успеха спрашивает:
`y` — Approve (один коммит в новую ветку `edy/…`, `agent.BranchName`), иначе — Reject (откат к снимку №0).
По сравнению с доской здесь **нет** семантического поиска, ревью, памяти проекта, MCP для исполнителя,
вложений и настроек проекта. Проект должен быть git-репозиторием без незакоммиченных изменений.
```sh
edy task -model agy/claude-sonnet-4-6 -project ~/src/demo "Добавь функцию Sub"
edy task -model ollama/llama3.1,anthropic/claude-opus-5-5 "Почини тесты"
```

### 4.11. `edy budget set <USD>` / `edy spend`
- `budget set` (`:108-113`) — дневной лимит в настройке `omniroute.SettingDailyBudget`; `0` — без лимита.
  Отрицательные и нечисловые значения отклоняются.
- `spend` (`:114-120`) — сумма расходов с локальной полуночи: `Сегодня потрачено: $0.1234`.

---

## 5. Окно приложения `cmd/edy-app` (Wails v2)

Два файла с взаимоисключающими build-тегами:
- `cmd/edy-app/main.go` (`//go:build desktop`) — настоящее окно;
- `cmd/edy-app/stub.go` (`//go:build !desktop`) — заглушка: печатает, как собрать окно, и выходит с кодом 1
  (`stub.go:12`). Благодаря ей `go build ./...`, `go vet ./...` и `go test ./...` работают без CGO и без Wails.

Как работает окно (`main.go`):
1. `start(log)` (`:59`): `app.OpenStore()`, затем `app.Start(st, home, DefaultAddr, log)`.
   - Успех: адрес `srv.URL() + "&app=1"`.
   - Порт занят, но там отвечает Edy (`app.Running`): окно подключается к уже запущенному `edy serve`,
     токен берётся из своей базы (`EnsureToken`), сервер не принадлежит окну (`srv == nil`).
   - Иначе ошибка, которая будет показана в окне.
2. `wails.Run(&options.App{…})` (`:31-50`): заголовок `Edy`, 1440×900, минимум 900×600, фон `#EEF0F7`,
   `SingleInstanceLock{UniqueId: "dev.edy.ide"}` (повторный запуск показывает уже открытое окно),
   `OnShutdown` → `srv.Close()`, `Mac.TitleBar = TitleBarDefault()`, About «Лёгкая агентная ИИ-IDE».
3. `AssetServer.Handler = bootstrap(target, startErr)` (`:81`): Wails отдаёт одну стартовую HTML-страницу,
   которая делает `location.replace("http://127.0.0.1:11435/#token=…&app=1")`, или страницу «Edy не запустился»
   с текстом ошибки. **Wails здесь — просто оболочка WKWebView**: биндингов Go↔JS нет, весь интерфейс
   приходит с локального HTTP-сервера, как в браузере.
4. Интерфейс видит `app=1` (`internal/ui/static/index.html:879`), запоминает его в `sessionStorage`
   и в режиме окна перехватывает клики по внешним ссылкам, отправляя их в `POST /api/open` (`index.html:930-940`).
   Сервер открывает такие ссылки через `open`/`xdg-open` (`internal/api/api.go:664`).
5. Экран «Подключение» показывает путь к CLI `edy` для MCP по stdio. Внутри `Edy.app` это
   `Contents/Resources/bin/edy` (`edyPath()`, `internal/api/api.go:131`).

Мелочи: `*store.Store`, открытый в `start`, никогда не закрывается (только `srv.Close()`). Если окно
подключилось к чужому `edy serve`, оба процесса держат одну базу SQLite одновременно (WAL это допускает).

---

## 6. Сборка

### 6.1. Требования
- Go **1.26+** (`go.mod:3`). В песочнице разработки стоял `go1.26.0 linux/amd64`.
- `git` (Shadow Git и операции проекта вызывают системный `git`, `internal/agent/shadow.go:37`).
- Для хуков проектов: `go` (vet/test), `python3`, `ruff`, `pytest` — по необходимости.
- Ядро собирается **без CGO**: SQLite — `modernc.org/sqlite`, PTY — `creack/pty`, Keychain — `go-keyring`
  (на macOS вызывает `/usr/bin/security`, CGO не нужен).

### 6.2. Локальная разработка
```sh
go build ./...                    # всё, включая заглушку cmd/edy-app
go build -o edy ./cmd/edy         # бинарник в корне (в .gitignore: /edy)
go vet ./...
gofmt -l .                        # должен ничего не печатать
go test ./...
go test -race ./internal/tasks    # гонки в менеджере задач (нужен CGO и gcc — для -race это норма)
```
На момент написания `go vet ./...` и `gofmt -l .` чистые, тесты `./internal/app`, `./cmd/...` и
`-race ./internal/tasks` проходят на Linux.

**Запуск на Linux.** Для любой команды, кроме справки, нужна Secret Service (D-Bus + gnome-keyring или KWallet).
В голом контейнере `edy` падает так:
```
ошибка: vault: не удалось прочитать мастер-ключ из Keychain: exec: "dbus-launch": executable file not found in $PATH
```
Поэтому на Linux вживую Edy запускали только через тесты или временный `cmd/devui`, где `vault.New(...)`
получает фиксированный ключ (см. §7.3). Обходного флага или переменной окружения для мастер-ключа нет.

### 6.3. Кросс-сборка CLI для Mac (так делались все релизы)
```sh
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o edy-macos-amd64 ./cmd/edy   # Intel
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o edy-macos-arm64 ./cmd/edy   # Apple Silicon
```
- Имена `edy-macos-*` уже в `.gitignore`.
- Компоновщик Go сам ставит ad-hoc подпись на darwin/arm64, поэтому бинарник запускается на M-серии.
- Файл, переданный через браузер или мессенджер, получает атрибут карантина. Перед запуском:
  `chmod +x edy && xattr -d com.apple.quarantine edy`.
- Без CGO собирается только CLI/сервер (`edy ui` даёт тот же интерфейс в браузере). Окно — только на Mac (§6.4).

**Релизы.** Сборки, которые получал владелец, назывались `edy-vNN` (последняя — **`edy-v23`**, darwin/amd64)
и передавались файлом. GitHub Releases и git-теги **не используются** (`git tag` пуст). Версия в бинарник
не зашивается (нет `-X main.version` и команды `version`), поэтому номер сборки знает только имя файла.
Следующую сборку логично назвать `edy-v24`.

### 6.4. Edy.app: `scripts/build-macos-app.sh`
Запускается **только на Mac**: нужны Xcode Command Line Tools, CGO и системные WebKit/Cocoa.
Wails CLI (`wails build`) **не нужен**: скрипт вызывает `go build` напрямую с нужными тегами.

Шаги (номера строк скрипта):
1. `set -euo pipefail`, переход в корень репозитория (`:3-5`).
2. Проверка `go` (`:7-10`) и `xcode-select -p` (`:11-14`) с подсказками установки
   (`brew install go`, `xcode-select --install`).
3. `VERSION=$(git describe --tags --always || echo dev)` (`:16`). Тегов нет, поэтому версия — короткий хеш
   коммита, и он же попадает в `CFBundleShortVersionString`. Формально это не номер `X.Y.Z`, но macOS
   такое терпит.
4. `rm -rf build/Edy.app`, создание `Contents/MacOS` и `Contents/Resources/bin` (`:17-19`).
5. Сборка окна (`:22-25`):
   `CGO_ENABLED=1 CGO_LDFLAGS="-framework UniformTypeIdentifiers" MACOSX_DEPLOYMENT_TARGET=13.0`
   `go build -tags desktop,production -trimpath -ldflags "-s -w" -o Contents/MacOS/Edy ./cmd/edy-app`.
   - `desktop` — тег проекта (включает `main.go` вместо `stub.go`);
   - `production` — тег Wails: отключает dev-сервер и инструменты разработчика;
   - `-framework UniformTypeIdentifiers` — без него на новых SDK не находятся символы UTType, которые использует Wails;
   - `13.0` — минимальная macOS, которую поддерживает Go 1.26 (см. коммит `6766944`).
6. Сборка CLI (`:30`): `CGO_ENABLED=0 go build … -o Contents/Resources/bin/edy ./cmd/edy`. CLI лежит
   **не** в `Contents/MacOS`: файловая система Mac не различает регистр, и `edy` перезаписал бы `Edy`
   (баг, исправленный в коммите `a6862cf`).
7. Защита от этой ошибки: `cmp -s` окна и CLI; если файлы одинаковые, сборка завершается с ошибкой (`:33-36`).
8. `Info.plist` (`:38-61`):
   | Ключ | Значение |
   |---|---|
   | `CFBundleName` / `CFBundleDisplayName` | `Edy` |
   | `CFBundleIdentifier` | `dev.edy.ide` (тот же, что сервис Keychain и `SingleInstanceLock`) |
   | `CFBundleExecutable` | `Edy` |
   | `CFBundlePackageType` | `APPL` |
   | `CFBundleShortVersionString`, `CFBundleVersion` | `${VERSION}` |
   | `LSMinimumSystemVersion` | `13.0` |
   | `NSHighResolutionCapable` | `true` |
   | `NSMicrophoneUsageDescription` | «Edy записывает голосовые сообщения и диктовку для чата с моделями.» |
   | `NSSpeechRecognitionUsageDescription` | «Edy превращает речь в текст в поле ввода.» |
   | `NSAppTransportSecurity → NSAllowsLocalNetworking` | `true` (окно грузит `http://127.0.0.1`) |
   Иконки (`CFBundleIconFile`) нет: у приложения стандартная иконка.
9. Ad-hoc подпись: `codesign --force --deep --sign - build/Edy.app`, ошибки игнорируются (`|| true`, `:64`).
   Нотаризации и Developer ID нет.
10. Вывод подсказок (`:66-70`): `open build/Edy.app`; установка
    `rm -rf /Applications/Edy.app && cp -R build/Edy.app /Applications/`; путь к CLI.

Собирается приложение под архитектуру того Mac, на котором запущен скрипт (universal-сборки нет).
`build/` в `.gitignore`.

---

## 7. Тесты

### 7.1. Общее
```sh
go test ./...
go test -race ./internal/tasks
go test ./internal/app -run TestOrchestratorGivesMCPToClaude -v
```
Тесты не трогают Keychain: везде `vault.New(bytes.Repeat([]byte{N}, 32))` и `t.TempDir()`.

### 7.2. Тесты `internal/app` и `cmd/edy`
| Тест | Файл:строка | Что проверяет |
|---|---|---|
| `TestStartServesUIAndAPI` | `internal/app/app_test.go:15` | `Start` на `127.0.0.1:0`: `/healthz`, формат `URL()` (`#token=edy-`), отдача `<title>Edy</title>`, `/api/agents` с токеном, пометка старого агента «прерван», ошибка второго `Start` на занятом порту, `Close` и `Done()` без ошибки |
| `TestMain` / `fakeClaude` | `internal/app/mcp_e2e_test.go:22`, `:33` | тестовый бинарник с `EDY_FAKE_CLAUDE=1` притворяется Claude Code: проверяет флаги `--strict-mcp-config`, `--allowedTools mcp__edy`, `--disallowedTools Bash Edit Write`, читает `--mcp-config` и через MCP Edy вызывает `write_file` |
| `TestOrchestratorGivesMCPToClaude` | `internal/app/mcp_e2e_test.go:101` | **e2e**: git-проект на Go, CLI-провайдер `cc` с командой `claude` (симлинк на тестовый бинарник в `PATH`), внешний MCP `docs`; `POST /api/tasks` с планом `MCP: docs`; ждёт `done` до 90 с; проверяет ленту (выдача MCP, снимок, `✓ go test`), создание `sub.go`, удаление временных `edy-mcp-*.json` с токеном и отсутствие лишнего агента `RoleMCP`. Пропускается без `git` или `go` |
| `TestEnsureInstalled` | `cmd/edy/main_test.go:13` | отказ, согласие («да») и уже установленная утилита |
| `TestMCPBridgeToRunningEdy` | `cmd/edy/mcp_test.go:18` | мост stdio→`/mcp` на `httptest`-сервере, сессия в журнале агентов закрыта по EOF |
| `TestMCPWithoutRunningEdy` | `cmd/edy/mcp_test.go:56` | автономный режим (`-addr 127.0.0.1:1`), в `tools/list` есть `project_skeleton` |

### 7.3. Проверка интерфейса (Playwright)
Автотестов UI нет. Каждое изменение интерфейса проверялось так:
1. Временный `cmd/devui/main.go` открывает базу во временной папке с фиксированным `vault.New`, добавляет
   фейковых провайдеров (httptest-серверы с SSE или эмбеддингами), задаёт `router_token` и вызывает
   `app.Start(st, home, "127.0.0.1:18080", nil)`.
2. Скрипт Playwright на Node открывает `http://127.0.0.1:18080/#token=…`, проходит сценарий, снимает
   скриншоты в светлой и тёмной теме и собирает ошибки консоли.
3. `cmd/devui` удаляется перед каждым коммитом. Подробно — в `docs/dev/07-ui.md`.

---

## 8. Платформенные особенности

- **Мастер-ключ.** `vault.FromKeychain()` (`internal/vault/vault.go:46`) через `zalando/go-keyring`:
  сервис `dev.edy.ide`, учётная запись `master-key`, значение — base64 от 32 случайных байт. Если записи нет,
  она создаётся. На macOS при первом доступе (или после пересборки с другой подписью) система может спросить
  разрешение на доступ к связке ключей. На Linux нужна Secret Service по D-Bus, без неё Edy не стартует (§6.2).
  На Windows go-keyring использует Credential Manager (`wincred`), но Windows-сборка не поддерживается и не проверялась.
- **Уведомления.** `notify.System` (`internal/notify/notify.go:27`) работает только на `darwin`
  (`osascript`), на других ОС молча ничего не делает. Отключаются настройкой `notify_off = "1"`.
- **`edy ui`** открывает браузер командой `open`, то есть только на macOS.
- **CLI-провайдеры** запускаются в PTY и устанавливаются через `/bin/bash -c` — нужна POSIX-система.
- **Chromium/Chrome** для веб-чатов: на macOS ищется также `~/Applications/Google Chrome.app`
  (`internal/webchat/chrome.go:53`).
- **Порт 11435 только на 127.0.0.1**: доступ с других машин не предусмотрен, все маршруты, кроме `/healthz`
  и `/`, требуют Bearer-токен.
- **In-process клиент** `agent.InProcess` (`internal/agent/llm.go:170`) использует `httptest.NewRecorder`:
  ответ OmniRoute целиком буферизуется, поэтому агенты внутри процесса получают поток SSE не по токенам,
  а одним куском после завершения ответа. Внешние клиенты и UI получают настоящий поток.

---

## 9. Справочник экспортируемых сущностей

### `internal/app`
| Имя | Файл:строка | Описание |
|---|---|---|
| `Home() (string, error)` | `internal/app/app.go:34` | папка данных: `$EDY_HOME` или `UserConfigDir()/Edy`, создаётся с `0700` |
| `OpenStore() (*store.Store, string, error)` | `internal/app/app.go:47` | `Home()` + мастер-ключ из Keychain + `store.Open(home/edy.db)`; возвращает базу и папку |
| `EnsureToken(st, home) (string, error)` | `internal/app/app.go:62` | токен из настройки `router_token` (создаётся при отсутствии) + запись `router.token` 0600 |
| `type Server` | `internal/app/app.go:78` | запущенный Edy; экспортированные поля `Addr` (фактический адрес) и `Token` |
| `(*Server) URL() string` | `internal/app/app.go:88` | `http://<Addr>/#token=<Token>`: токен в `#`-части не уходит на сервер |
| `(*Server) Done() <-chan error` | `internal/app/app.go:91` | канал, который получает ошибку `Serve` (или `nil` после `Close`) и закрывается |
| `Start(st, home, addr, log) (*Server, error)` | `internal/app/app.go:94` | собирает и запускает все компоненты (§2.1) |
| `(*Server) Close()` | `internal/app/app.go:188` | `Shutdown` (5 с), закрытие MCP-сессий и браузеров веб-чатов |
| `Running(addr string) bool` | `internal/app/app.go:197` | `GET http://addr/healthz` с таймаутом 1 с отвечает 200 |
| `settingRouterToken` (неэкспорт.) | `internal/app/app.go:31` | имя настройки `router_token` |

### `cmd/edy` (пакет `main`; экспортированных сущностей нет — перечислены все функции)
| Имя | Файл:строка | Описание |
|---|---|---|
| `usage` | `cmd/edy/main.go:33` | текст справки |
| `main` | `cmd/edy/main.go:57` | вызывает `run`, при ошибке пишет `ошибка: …` и выходит с кодом 1 |
| `run` | `cmd/edy/main.go:64` | открывает базу и разбирает подкоманду |
| `openStore` / `ensureToken` | `cmd/edy/main.go:127`, `:129` | обёртки над `app.OpenStore` / `app.EnsureToken` |
| `serve` | `cmd/edy/main.go:131` | `edy serve` и `edy ui` |
| `providerAdd` | `cmd/edy/main.go:160` | `edy provider add` |
| `ensureInstalled` | `cmd/edy/main.go:231` | находит или устанавливает утилиту CLI-профиля |
| `runShell` | `cmd/edy/main.go:255` | `/bin/bash -c` с терминалом пользователя |
| `presetNames` | `cmd/edy/main.go:261` | отсортированный список профилей для справки флага |
| `webLogin` | `cmd/edy/main.go:273` | `edy web login` |
| `runTask` | `cmd/edy/main.go:302` | `edy task` |
| `firstLineOf` | `cmd/edy/main.go:375` | первая строка текста задачи для сообщения коммита |
| `providerRemove` | `cmd/edy/main.go:383` | `edy provider remove` |
| `providerList` | `cmd/edy/main.go:403` | `edy provider list` |
| `keyAdd` | `cmd/edy/main.go:424` | `edy key add` |
| `runMCP` | `cmd/edy/mcp.go:28` | `edy mcp`: мост или автономный сервер |
| `bridge` | `cmd/edy/mcp.go:62` | состояние моста: адрес, токен, проект, `Mcp-Session-Id` |
| `(*bridge) register` | `cmd/edy/mcp.go:70` | `POST /api/projects` |
| `(*bridge) serve` | `cmd/edy/mcp.go:92` | цикл чтения stdin (буфер 1 МБ) |
| `(*bridge) forward` | `cmd/edy/mcp.go:112` | пересылка одного JSON-RPC сообщения в `/mcp` |
| `(*bridge) close` | `cmd/edy/mcp.go:146` | `DELETE /mcp` для сессии |
| `rpcFailure` | `cmd/edy/mcp.go:159` | JSON-RPC ошибка `-32603`, для уведомлений — `nil` |

### `cmd/edy-app`
| Имя | Файл:строка | Описание |
|---|---|---|
| `main` (desktop) | `cmd/edy-app/main.go:27` | `wails.Run` с настройками окна |
| `start` | `cmd/edy-app/main.go:59` | поднимает Edy или подключается к запущенному |
| `bootstrap` | `cmd/edy-app/main.go:81` | стартовая страница: редирект в интерфейс или текст ошибки |
| `main` (заглушка) | `cmd/edy-app/stub.go:12` | подсказка про `scripts/build-macos-app.sh`, выход с кодом 1 |

---

## 10. Зависимости (`go.mod`)

### Прямые
| Модуль | Версия | Зачем | Где |
|---|---|---|---|
| `github.com/anthropics/anthropic-sdk-go` | v1.82.0 | адаптер Anthropic Messages API (перевод формата, SSE) и загрузка списка моделей | `internal/omniroute/anthropic.go`, `models.go` |
| `github.com/chromedp/chromedp` | v0.16.0 | управление Chromium/Chrome по CDP для веб-чатов | `internal/webchat/manager.go` |
| `github.com/chromedp/cdproto` | v0.0.0-20260714… | низкоуровневые типы CDP (`Input.insertText` и т. п.) | `internal/webchat/manager.go` |
| `github.com/creack/pty` | v1.1.24 | запуск CLI-утилит в псевдотерминале без CGO | `internal/omniroute/cli.go` |
| `github.com/wailsapp/wails/v2` | v2.16.0 | окно приложения (WKWebView); только под тегом `desktop` | `cmd/edy-app/main.go` |
| `github.com/zalando/go-keyring` | v0.2.8 | мастер-ключ в Keychain / Secret Service | `internal/vault/vault.go` |
| `modernc.org/sqlite` | v1.60.1 | SQLite на чистом Go (без CGO) | `internal/store/store.go` |

### Косвенные (по происхождению)
- **modernc.org/sqlite**: `modernc.org/libc`, `mathutil`, `memory`, `github.com/dustin/go-humanize`,
  `github.com/google/uuid`, `github.com/ncruces/go-strftime`, `github.com/remyoudompheng/bigfft`, `golang.org/x/sys`.
- **anthropic-sdk-go**: `github.com/tidwall/{gjson,match,pretty,sjson}`, `github.com/invopop/jsonschema`,
  `github.com/bahlo/generic-list-go`, `github.com/buger/jsonparser`, `github.com/pb33f/ordered-map/v2`,
  `go.yaml.in/yaml/v4`, `github.com/standard-webhooks/...`, `go.opentelemetry.io/{otel,metric,trace,auto/sdk}`,
  `github.com/go-logr/{logr,stdr}`.
- **chromedp**: `github.com/chromedp/sysutil`, `github.com/gobwas/{ws,httphead,pool}`,
  `github.com/go-json-experiment/json`.
- **go-keyring**: `github.com/godbus/dbus/v5` (Linux), `github.com/danieljoos/wincred` (Windows).
- **wails**: `github.com/labstack/{echo/v4,gommon}`, `github.com/leaanthony/*`, `github.com/bep/debounce`,
  `github.com/samber/lo`, `github.com/tkrajina/go-reflector`, `github.com/wailsapp/{go-webview2,mimetype}`,
  `github.com/jchv/go-winloader`, `github.com/go-ole/go-ole`, `git.sr.ht/~jackmordaunt/go-toast/v2`,
  `github.com/pkg/{browser,errors}`, `github.com/gorilla/websocket`, `github.com/mattn/{go-colorable,go-isatty}`,
  `github.com/rivo/uniseg`, `github.com/valyala/{bytebufferpool,fasttemplate}`, `golang.org/x/{crypto,net,text,sync}`.

Происхождение любой зависимости можно проверить так: `go mod why -m <модуль>`.
**go-git, chromem-go и Tree-sitter в проекте нет**, хотя `docs/ARCHITECTURE.md` их упоминает (см. §12).

---

## 11. `.gitignore` и мусор в репозитории

`.gitignore` (13 строк): `*.db`, `*.db-wal`, `*.db-shm`, `router.token`, `.env`, `.env.*`, `*.pem` — данные и
секреты; `/edy`, `/edy-macos-*`, `/build/` — артефакты сборки.

Замечания:
- В корне репозитория **закоммичен** файл `home (1).png` (скриншот, около 218 КБ). Скорее всего, он попал
  туда случайно.
- В рабочей копии есть незакоммиченный `edy` (около 31 МБ, linux/amd64, с отладочной информацией). Он игнорируется.
- `cmd/devui/` в `.gitignore` не внесён: его нужно удалять вручную перед коммитом (§7.3). Можно добавить
  `/cmd/devui/` в `.gitignore`.

---

## 12. README.md и docs/ARCHITECTURE.md

### README.md (78 строк) — кратко
Описание продукта («лёгкая агентная ИИ-панель для macOS»), сборка окна (`git clone -b claude/elegant-edison-gu1b8f …`,
`scripts/build-macos-app.sh`, `open build/Edy.app`), `edy ui`, быстрый старт из терминала (провайдеры
anthropic/openai/cli, `budget set`, `serve`), пример `curl` к `/v1/chat/completions`, подключение MCP
(`claude mcp add edy -- edy mcp -project "$PWD"`), `go test ./...`.

Устарело или неточно:
- «Сейчас готов **OmniRoute**…» — на деле уже есть Оркестратор, доска задач, эпики, команда, память, ревью,
  поиск по смыслу, MCP, веб-чаты, окно Wails.
- Не упомянуты `edy task`, `edy web …`, `edy provider remove/list`, `edy spend`, `-kind web`.
- Путь `~/Library/Application Support/Edy/router.token` верен только для macOS; не сказано про `EDY_HOME`.
- Нет кросс-сборки, `go vet`, `-race`, требований к Keychain на Linux.

### docs/ARCHITECTURE.md (446 строк) — что покрывает
Рамки продукта и технические решения (без CGO, токен, Shadow Git, бюджет, `X-Edy-Client`), агентный цикл
(`edy task`, Circuit Breaker, Approve/Reject), Оркестратор и проекты, вкладка «Git», журнал агентов,
MCP-сервер и скиллы (каталог внешних MCP, выдача Оркестратором), доска задач, эпик, команда, память,
картинки и звук, Context Engine и поиск по смыслу, окно Wails, каталог провайдеров (13 CLI, 20 API),
этап 1 OmniRoute (поток запроса, ротация ключей, ограничения Anthropic), CLI-адаптер, веб-интерфейс,
web-адаптер (Chromium/Chrome, вход в два этапа, диагностика), «Следующие шаги».

Устаревшие места:
- Таблица решений: «Shadow Git … (go-git)» — на самом деле вызывается системный `git`
  (`internal/agent/shadow.go:37`); «векторы в `chromem-go`» — векторы лежат в SQLite (таблица `embeddings`);
  «Headless-браузер — установленный Chrome» — по умолчанию скачивается изолированный Chromium.
- «Языки проектов: … грамматики Tree-sitter» противоречит строке про CGO: используются `go/ast` и `python3 ast`.
- «Задачи хранятся в памяти процесса: после перезапуска лента пропадает» (раздел Оркестратора) — теперь
  задачи хранятся в SQLite (`board_tasks`, `board_events`) и восстанавливаются `Recover()`.
- «Этап 1 … `cmd/edy/` CLI: serve, token, provider, key, budget, spend» — неполный список (нет `ui`, `web`, `task`, `mcp`).
- «`edy task`: Shadow Git в `.edy/shadow`» и «`.edy/shadow.git`» — в документе встречаются оба варианта; в коде `.edy/shadow.git`.
- «Следующие шаги этапа 1, п. 2: окно Wails» — уже сделано.
- Комментарий пакета `cmd/edy` (`cmd/edy/main.go:1-2`: «Пока UI на Wails не готов…») тоже устарел.

---

## 13. Ограничения, непроверенное и идеи

### Известные ограничения
- Мастер-ключ только из связки ключей; на Linux без D-Bus/Secret Service Edy не запускается, переменной
  для ключа нет.
- Любая подкоманда `edy` (включая `mcp`) открывает базу и Keychain.
- `Server.Close()` не останавливает и не ждёт задачи агентов; при следующем старте они откатываются.
- In-process клиенты получают ответы буферизованно (`httptest.NewRecorder`).
- Нет версии в бинарнике, тегов, CI, universal-сборки, нотаризации, иконки приложения.
- Окно и CLI могут работать одновременно; окно тогда только подключается к `edy serve`, и, если закрыть
  терминал, окно останется без сервера.
- Адрес `127.0.0.1:11435` зашит в окне (`DefaultAddr`) и продублирован строкой в `cmd/edy/mcp.go:31`.

### Не проверено на реальной системе
- **Веб-чаты** (ChatGPT, Claude.ai, Gemini): селекторы на реальных аккаунтах не проверялись.
- **Настоящий Claude Code с MCP Edy**: e2e-тест использует поддельный `claude`; реальный запуск
  с `--mcp-config`/`--strict-mcp-config` не проверялся. Из CLI-профилей вживую проверялись только Antigravity и установка.
- **Импорт задач из issue GitHub/GitLab/Gitea** (`POST /api/board/issue`, `internal/api/projects.go:603`) —
  только на тестовых серверах.
- **Уведомления macOS** через `osascript` — на Mac вживую не подтверждены (в Linux-песочнице это пустая операция).
- **Микрофон и диктовка в окне Wails**: в `Info.plist` есть `NSMicrophoneUsageDescription` и
  `NSSpeechRecognitionUsageDescription`, но WKWebView в Wails v2 может не выдавать странице доступ
  к `getUserMedia` и Web Speech API. Интерфейс на этот случай показывает подсказку открыть `edy ui`
  в браузере (`internal/ui/static/index.html:1758`). Поведение на реальном Mac не проверено.
- **Сборка Edy.app** на актуальной macOS и Xcode: скрипт проверялся владельцем; в песочнице (Linux) он не запускается.

### Идеи (TODO)
- `edy version` и `-ldflags "-X main.version=v24"`; git-теги `vNN` вместо ручных имён файлов.
- Скрипт `scripts/release.sh`: две архитектуры CLI, `lipo` в universal, архив, контрольные суммы.
- Переменная вроде `EDY_MASTER_KEY_FILE` для Linux и CI, чтобы не зависеть от Secret Service.
- Открывать базу лениво: не трогать Keychain в `edy mcp` и при выводе справки подкоманд.
- В `Server.Close()` отменять задачи и вызывать `Tasks.Wait()` с таймаутом.
- Логировать в `Start` фактический адрес `ln.Addr()`; в `mcp.go` использовать `omniroute.DefaultAddr`.
- `edy ui` на Linux — `xdg-open` (как в `internal/api/api.go:664`).
- Иконка приложения (`CFBundleIconFile`), universal-бинарник окна, подпись Developer ID и нотаризация, если
  приложение будут передавать другим людям.
- Удалить `home (1).png` из репозитория и добавить `/cmd/devui/` в `.gitignore`.
- Обновить README и ARCHITECTURE по списку из §12.
