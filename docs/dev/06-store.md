# 06. Хранилище Edy (`internal/store`) — модель данных

Справочник по базе данных Edy: где она лежит, какие в ней таблицы, как они создаются и
мигрируются, какие колонки зашифрованы, какие ключи настроек существуют и кто читает/пишет
каждую таблицу. Маршруты API, которые всё это вызывают, — в [05-api.md](05-api.md).

Ссылки `файл:строка` даны относительно `internal/store/`, если не указано иное.

---

## 1. Обзор

* Движок — **SQLite** через драйвер `modernc.org/sqlite` (чистый Go, без CGO; импорт в `store.go:14`).
  Это важно: ядро Edy собирается без CGO (см. `docs/ARCHITECTURE.md`).
* Один файл базы: `<Home>/edy.db` (раздел 2).
* Один тип-обёртка `Store` (`store.go:116-119`) с полями `db *sql.DB` и `vault *vault.Vault`.
  Все методы — на `*Store`, разнесены по файлам по предметным областям:

| Файл | Строк | Что внутри |
|---|---|---|
| `store.go` | 380 | `Open`, базовая схема (`providers`, `api_keys`, `settings`, `usage`), миграции, провайдеры, ключи, настройки, расходы |
| `agents.go` | 245 | журнал агентов (`agents`, `agent_events`), статистика моделей |
| `git.go` | 153 | аккаунты git-хостингов (`git_accounts`), настройки проекта (`ProjectSettings` в `settings`) |
| `board.go` | 153 | доска задач (`board_tasks`, `board_events`) |
| `memory.go` | 95 | память проекта (`project_memory`) |
| `team.go` | 113 | постоянные агенты команды (`team`) |
| `mcpservers.go` | 139 | каталог внешних MCP-серверов (`mcp_servers`) |
| `embeddings.go` | 103 | векторы поиска по смыслу (`embeddings`) |
| `store_test.go`, `agents_test.go` | — | тесты |

* Ошибки-сигналы (`store.go:25-28`):
  * `ErrNotFound = errors.New("store: не найдено")` — нет строки / `RowsAffected() == 0`;
  * `ErrExists = errors.New("store: уже существует")` — только `AddProvider` при нарушении `UNIQUE`.
  Вызывающий код сравнивает их через `errors.Is`.
* Время хранится целыми числами: `usage.ts` — **секунды** Unix; все остальные таблицы (`agents`,
  `agent_events`, `board_*`, `project_memory`, `team`, `embeddings`) — **миллисекунды** Unix
  (`UnixMilli`). При ручных SQL-запросах это легко перепутать.

---

## 2. Где лежат данные

### 2.1. Папка данных (`Home`)

`app.Home()` — `internal/app/app.go:34-44`:

```go
home := os.Getenv("EDY_HOME")
if home == "" {
    cfg, _ := os.UserConfigDir()        // macOS: ~/Library/Application Support
    home = filepath.Join(cfg, "Edy")
}
return home, os.MkdirAll(home, 0o700)
```

* macOS: `~/Library/Application Support/Edy`. На Linux (разработка/CI) — `~/.config/Edy`.
* Переменная окружения `EDY_HOME` переопределяет путь целиком (удобно для тестов и второго экземпляра).
* Папка создаётся с правами `0700`.

`app.OpenStore()` (`internal/app/app.go:47-58`): `Home()` → `vault.FromKeychain()` →
`store.Open(<Home>/edy.db, vault)`. Используют и `edy serve`/`edy ui`, и окно Wails, и CLI-команды
(`cmd/edy/main.go`).

### 2.2. Содержимое папки данных

| Путь | Что это | Кто создаёт |
|---|---|---|
| `edy.db` | база SQLite | `store.Open` |
| `edy.db-wal`, `edy.db-shm` | журнал WAL (включён прагмой) | SQLite |
| `router.token` | копия токена OmniRoute/API + `\n`, права `0600` — для внешних программ (`edy mcp` читает его в `cmd/edy/mcp.go:46`) | `app.EnsureToken`, `internal/app/app.go:74` |
| `skills/<имя>/SKILL.md` | общие скиллы | API `/api/skills`, пользователь (`internal/skills`) |
| `attachments/<task_id>/<N>-<имя>.<ext>` | скриншоты-вложения задач (права `0700`/`0600`) | `tasks.Manager.Attach` (`internal/tasks/attachments.go`), `AttachDir` задаётся в `internal/app/app.go:122` |
| `repos/<имя>[-N]` | репозитории, клонированные через `POST /api/git/clone` | `ReposDir`, `internal/app/app.go:157` |
| `web-profiles/<провайдер>` | профиль браузера веб-чата (cookies, вход) — **не шифруется** | `webchat.Manager`, `internal/webchat/manager.go:161` |
| `web-debug/` | снимки страниц веб-чатов при ошибках (диагностика) | `internal/webchat/manager.go:825` |
| `chromium/` | скачанный изолированный Chromium | `internal/webchat/chrome.go:115` |

### 2.3. Данные внутри папки проекта

Не в базе, но важны для понимания: `<project>/.edy/shadow.git` (Shadow Git — снимки),
`<project>/.edy/skills/` (скиллы проекта), `<project>/.edy/worktrees/<epic>` (worktree эпиков),
`<project>/.edy/hooks.json` (свои хуки). `.edy/` добавляется в `.git/info/exclude`.

### 2.4. Мастер-ключ шифрования

Не в папке данных, а в **macOS Keychain**: сервис `dev.edy.ide`, аккаунт `master-key`, значение — 32 байта
в base64 (`internal/vault/vault.go:17-21`, `FromKeychain` `vault.go:46`). Генерируется при первом запуске.
**Если удалить запись Keychain или перенести `edy.db` на другой Mac без неё — все зашифрованные колонки
(ключи API, токены git, конфиги MCP) станут нечитаемыми** (`Keys`/`GitAccount`/`MCPServers` вернут ошибку
расшифровки). Остальные данные останутся доступны.

---

## 3. Открытие базы, схема и миграции

### 3.1. `Open`

`store.go:122-138`:

```go
db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
db.Exec(schema + agentsSchema + gitSchema + mcpSchema + boardSchema + memorySchema + teamSchema + embeddingsSchema)
for _, m := range migrations {
    if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") { … }
}
```

* Прагмы: `foreign_keys(1)` — каскадные удаления работают; `journal_mode(WAL)` — читатели не блокируют
  писателя; `busy_timeout(5000)` — ждать блокировку до 5 с вместо мгновенной `SQLITE_BUSY`.
* Схема — **конкатенация** строк-констант из всех файлов, каждая таблица/индекс — `CREATE … IF NOT EXISTS`.
  Выполняется одним `Exec` при каждом открытии; идемпотентно.
* Порядок конкатенации важен только для миграций (таблицы должны существовать до `ALTER`).
* `Close()` — `store.go:140`.

### 3.2. Миграции

`migrations` — `store.go:65-72`. Подход: **только добавление колонок** через `ALTER TABLE … ADD COLUMN`
с `DEFAULT`, выполняются при каждом открытии; ошибка с текстом `duplicate column` (колонка уже есть)
игнорируется. Версии схемы нет.

| # | Миграция | Зачем |
|---|---|---|
| 1 | `ALTER TABLE providers ADD COLUMN command TEXT NOT NULL DEFAULT '[]'` | CLI-провайдеры |
| 2 | `ALTER TABLE providers ADD COLUMN site TEXT NOT NULL DEFAULT ''` | веб-чаты |
| 3 | `ALTER TABLE providers ADD COLUMN browser TEXT NOT NULL DEFAULT ''` | вид браузера веб-чата |
| 4 | `ALTER TABLE providers ADD COLUMN caps TEXT NOT NULL DEFAULT ''` | теги возможностей |
| 5 | `ALTER TABLE mcp_servers ADD COLUMN account TEXT NOT NULL DEFAULT ''` | привязка сервера к аккаунту |
| 6 | `ALTER TABLE project_memory ADD COLUMN agent TEXT NOT NULL DEFAULT ''` | личная память агентов команды |
| 7 | `ALTER TABLE providers ADD COLUMN show_window INTEGER NOT NULL DEFAULT 0` | веб-чат работает в видимом окне |

Обратите внимание: в `CREATE TABLE` этих таблиц новых колонок **нет** — на свежей базе они тоже
появляются только через миграции. Поэтому полная схема таблицы = `CREATE` + её миграции.

**Как добавить изменение схемы:**
* новая таблица — новый `CREATE TABLE IF NOT EXISTS` в константе схемы своего файла и добавление
  константы в конкатенацию в `Open` (`store.go:127`);
* новая колонка — строка `ALTER TABLE … ADD COLUMN … NOT NULL DEFAULT …` в **конец** `migrations`
  (колонку **не** добавлять в `CREATE`, иначе на новой базе `ALTER` упадёт с `duplicate column` — что
  безопасно, но нарушает принятый стиль);
* переименование/удаление колонок и смена типов этим механизмом не поддерживаются — потребуется
  ввести версионирование (`PRAGMA user_version`).

---

## 4. Таблицы

Ниже для каждой таблицы: DDL (с учётом миграций), смысл полей, ключи и индексы, методы, кто пишет и читает.

### 4.1. `providers` — источники моделей OmniRoute

`store.go:31-39` + миграции 1–4.

| Колонка | Тип | Значение |
|---|---|---|
| `id` | INTEGER PK | rowid |
| `name` | TEXT NOT NULL **UNIQUE** | имя = префикс модели `<name>/<model>`; без `/` (проверяет API) |
| `kind` | TEXT NOT NULL | `openai` / `anthropic` / `cli` / `web` (`KindOpenAI…KindWeb`, `store.go:18-23`) |
| `base_url` | TEXT '' | адрес API |
| `models` | TEXT '' | список моделей **через запятую** (поэтому модель с `,` запрещена API) |
| `price_in`, `price_out` | REAL 0 | USD за 1M входных / выходных токенов |
| `command` | TEXT '[]' | JSON-массив аргументов CLI с `{model}`, `{prompt}` |
| `site` | TEXT '' | веб-чат (`chatgpt`, `claude`, `gemini`…) |
| `browser` | TEXT '' | `chromium` / `chrome` |
| `caps` | TEXT '' | JSON-массив тегов, заданных вручную; `''` = автоопределение (`Provider.Caps == nil`), `'[]'` = вручную «ничего» |
| `show_window` | INTEGER 0 | 1 — рабочий браузер веб-чата с окном; 0 — скрытый (`--headless=new`) |

Индексы: только `UNIQUE(name)`.

Методы: `AddProvider` (не пишет `caps`), `SetModels`, `SetCaps`, `SetShowWindow`, `DeleteProvider`, `Provider`, `Providers`
(`ORDER BY name`), внутренний `scanProvider` (`store.go:225`; ошибка, если `command` — не JSON).

Пишут: API `POST/DELETE /api/providers`, `PUT …/models`, `PUT …/caps` (`internal/api/api.go:464, 484, 244, 329`);
CLI `edy provider add/rm` (`cmd/edy/main.go:220, 394`).
Читают: OmniRoute (`internal/omniroute/server.go:112, 147`; `embeddings.go:36`), API (`api.go`, `projects.go`,
`gitapi.go:235`, `mcpapi.go:227`), `internal/app/app.go:125` (тег vision), CLI (`main.go:274, 389, 404, 431`).

### 4.2. `api_keys` — ключи провайдеров (зашифрованы)

`store.go:40-46`.

| Колонка | Тип | Значение |
|---|---|---|
| `id` | INTEGER PK | порядок ротации ключей (по возрастанию) |
| `provider_id` | INTEGER NOT NULL → `providers(id)` **ON DELETE CASCADE** | |
| `label` | TEXT '' | метка для интерфейса |
| `secret` | **BLOB** NOT NULL | `vault.Seal(ключ)` = `nonce(12) ‖ AES-256-GCM(ключ)` |
| `disabled` | INTEGER 0 | 1 — ключ выключен (например, после 401/403 от провайдера) |

Методы: `AddKey` (шифрует), `Keys` (только `disabled = 0`, расшифровывает, `ORDER BY id`),
`KeyInfos` (все ключи, без секретов), `DeleteKey`, `DisableKey`.

Пишут: API `POST /api/providers/{name}/keys`, `DELETE /api/keys/{id}`; CLI `edy key add` (`main.go:444`);
OmniRoute выключает ключ — `DisableKey` (`internal/omniroute/server.go:287`, `embeddings.go:98`).
Читают: OmniRoute (`server.go:255`, `embeddings.go:58`), API (`api.go:211` — remote-models, `api.go:348` — KeyInfos),
CLI (`main.go:411`).
Включить ключ обратно (`disabled = 0`) нечем — метода нет.

### 4.3. `settings` — ключ-значение

`store.go:47-50`: `key TEXT PRIMARY KEY`, `value TEXT NOT NULL`.
Методы: `Setting(key)` (`store.go:325`; `ErrNotFound`, если нет), `SetSetting(key, value)` (`store.go:334`;
upsert `ON CONFLICT(key) DO UPDATE`). Все ключи — раздел 5.

### 4.4. `usage` — учёт расходов токенов

`store.go:51-61`.

| Колонка | Тип | Значение |
|---|---|---|
| `id` | INTEGER PK | |
| `ts` | INTEGER NOT NULL | **секунды** Unix (`time.Now().Unix()`) |
| `provider` | TEXT NOT NULL | имя провайдера |
| `model` | TEXT NOT NULL | модель без префикса провайдера |
| `client` | TEXT '' | заголовок `X-Edy-Client`: `orchestrator`, `agent`, `reviewer`, `embeddings`, `chat`, пусто/иное — внешние программы |
| `input_tokens`, `output_tokens` | INTEGER NOT NULL | |
| `cost_usd` | REAL NOT NULL | `Provider.Cost(in, out)` = `(in·price_in + out·price_out)/1e6` (`store.go:95`) |

Индекс: `usage_ts ON usage(ts)`.

Методы: `RecordUsage` (`store.go:339`), `UsageSince` (`store.go:357`, группировка по client/provider/model,
дорогие первыми), `SpentSince` (`store.go:376`, сумма), а также `ModelStats` (agents.go) читает её.

Пишет: только OmniRoute (`internal/omniroute/server.go:213`, `embeddings.go:111`).
Читают: OmniRoute `checkBudget` (`server.go:240`), API `/api/usage`, `/api/stats/models`, Оркестратор
(`orchestratorContext`), CLI `edy spend` (`main.go:116`). Записи не удаляются никогда.

### 4.5. `agents` — журнал агентов

`agents.go:28-41`.

| Колонка | Тип | Значение |
|---|---|---|
| `id` | TEXT PK | см. ниже форматы |
| `role` | TEXT NOT NULL | `orchestrator`, `executor`, `mcp`, `reviewer`, `micro` (`agents.go:11-16`) |
| `model` | TEXT NOT NULL | `провайдер/модель`; у MCP-клиента — «имя версия» клиента |
| `project` | TEXT '' | путь проекта |
| `task_id` | TEXT '' | задача доски (для исполнителей и ревьюеров) |
| `goal` | TEXT '' | цель / первая реплика |
| `state` | TEXT NOT NULL | `running`, `done`, `failed`, `cancelled`, `interrupted` (`agents.go:19-25`) |
| `summary` | TEXT '' | итог |
| `error` | TEXT '' | ошибка |
| `created` | INTEGER NOT NULL | мс |
| `finished` | INTEGER 0 | мс; 0 — ещё работает |

Индекс `agents_created ON agents(created)`.

Форматы `id`:
* `orch-<conversation>` — Оркестратор одного разговора (`internal/api/projects.go:408`);
* `<task_id>-<N>` — N-я модель, взявшая задачу (`internal/tasks/tasks.go:1034`);
* `<task_id>-review` — ревьюер (`internal/review/review.go:47`);
* `mcp-<16 hex>` — внешний MCP-клиент (`internal/api/mcpapi.go:182`).

Методы: `CreateAgent` (`agents.go:76`; по умолчанию `Created=now`, `State=running`), `FinishAgent` (`:89`),
`TouchAgent` (`:98`; для «урывочных» агентов — при `running` сбрасывает `finished` в 0),
`InterruptRunningAgents` (`:113`; при старте Edy все `running` → `interrupted`, ошибка «Edy был закрыт во время
работы»), `Agents(limit)` (`:139`, новые первыми, с числом событий подзапросом), `Agent(id)` (`:156`).

Пишут: `tasks.Manager` через интерфейс `tasks.Journal` (`internal/tasks/tasks.go:1036, 1062-1066`),
ревьюер (`internal/review/review.go:49, 60, 72`), API (`projects.go:411-413, 280-293`; `mcpapi.go:186, 213`),
`internal/app/app.go:159` (InterruptRunningAgents).
Читают: API `GET /api/agents`, `/api/agents/{id}`, `projects.go:410, 573`; `ModelStats`.
Записи не удаляются (растёт бесконечно; интерфейс показывает последние 200).

### 4.6. `agent_events` — события агентов

`agents.go:42-49`.

| Колонка | Тип | Значение |
|---|---|---|
| `id` | INTEGER PK | курсор для опроса (`after`) |
| `agent_id` | TEXT NOT NULL → `agents(id)` ON DELETE CASCADE | |
| `ts` | INTEGER NOT NULL | мс |
| `kind` | TEXT NOT NULL | `user`, `model`, `plan`, `tool`, `checkpoint`, `hooks`, `breaker`, `info`, `error`, `review` и др. |
| `text` | TEXT NOT NULL | |

Индекс `agent_events_agent ON agent_events(agent_id, id)`.
Методы: `AddAgentEvent` (`agents.go:107`), `AgentEvents(agentID, afterID)` (`:165`).
Из-за внешнего ключа событие для несуществующего агента не вставится (ошибка обычно игнорируется вызывающими).
Пишут: tasks (`tasks.go:233, 1028, 1049`), ревьюер (`review.go:67`), API (`projects.go`, `mcpapi.go:166, 169`).
Читает: `GET /api/agents/{id}` (`gitapi.go:336`).

### 4.7. `git_accounts` — аккаунты GitHub/GitLab/Gitea (токен зашифрован)

`git.go:16-25`.

| Колонка | Тип | Значение |
|---|---|---|
| `id` | INTEGER PK | на него ссылается `ProjectSettings.account_id` (без внешнего ключа) |
| `kind` | TEXT NOT NULL | `github` / `gitlab` / `gitea` (`git.go:10-14`) |
| `host` | TEXT NOT NULL | `https://github.com` и т. п., без `/` на конце |
| `label` | TEXT '' | |
| `username` | TEXT '' | |
| `token` | **BLOB** NOT NULL | `vault.Seal(токен)` |

Методы: `AddGitAccount` (`git.go:38`, шифрует), `GitAccounts` (`:50`, **без** токена — `Token` пустой,
к тому же `json:"-"`), `GitAccount(id)` (`:68`, с расшифрованным токеном), `DeleteGitAccount` (`:85`).
Пишут/читают: API `gitapi.go` (`35, 85, 95, 109, 298`), `projects.go:616, 625` (импорт issue).

### 4.8. `board_tasks` — карточки доски

`board.go:14-25`.

| Колонка | Тип | Значение |
|---|---|---|
| `id` | TEXT PK | `task_<hex>` (`internal/tasks/tasks.go:968`) |
| `project` | TEXT NOT NULL | путь проекта |
| `parent` | TEXT '' | родитель (подзадача эпика) |
| `title` | TEXT '' | дубль из JSON для удобства |
| `data` | TEXT NOT NULL | **JSON всей записи `tasks.record`** (`View` + служебные `baseline`, `all_skills`, `patch` ≤ 400 КБ, `epic_base`, `epic_branch`, `sub_branch`) — формат принадлежит пакету `tasks` |
| `state` | TEXT NOT NULL | колонка доски (`backlog`, `queued`, `running`, `done`, `needs_human`, `failed`, `cancelled`, `approved`, `rejected`, `merged`) |
| `position` | INTEGER 0 | порядок в колонке |
| `created`, `updated` | INTEGER NOT NULL | мс |

Индекс `board_tasks_project ON board_tasks(project, state)`.
Методы: `SaveBoardTask` (`board.go:59`, upsert; при конфликте обновляет всё, кроме `project` и `created`),
`BoardTask(id)` (`:87`), `BoardTasks(project)` (`:96`; пустой project — все; `ORDER BY position, created`),
`DeleteBoardTask` (`:119`; события удаляются каскадом). Тип `BoardTask` (`:38`) — `Data json.RawMessage`.
Единственный пользователь — `tasks.Manager` через интерфейс `tasks.Board` (`internal/tasks/tasks.go:34-41`):
`save` (`tasks.go:218`), `load` (`:200`), `List`/`Recover` (`:395`, `:929`), `epic.go:52, 136`, `tasks.go:917, 923`.

### 4.9. `board_events` — история карточки

`board.go:26-33`: `id` INTEGER PK, `task_id` TEXT → `board_tasks(id)` ON DELETE CASCADE, `ts` (мс), `kind`, `text`.
Индекс `board_events_task ON board_events(task_id, id)`.
`kind`: `created`, `queued`, `started`, `model`, `tool`, `checkpoint`, `hooks`, `breaker`, `plan`, `review`,
`comment`, `approved`, `subtask`, `info`, `error`.
Методы: `AddBoardEvent` (`board.go:130`), `BoardEvents(taskID, afterID)` (`:136`). Тип `BoardEvent` (`:51`,
с JSON-тегами — отдаётся в API как есть). Пишут/читают: tasks (`tasks.go:231, 1026, 529, 658`; `epic.go:112, 118`).

### 4.10. `project_memory` — память проекта

`memory.go:10-19` + миграция 6.

| Колонка | Тип | Значение |
|---|---|---|
| `id` | INTEGER PK | |
| `project` | TEXT NOT NULL | путь |
| `kind` | TEXT NOT NULL | `fact`, `lesson`, `task`, `note` (`memory.go:23-28`) |
| `text` | TEXT NOT NULL | ≤ 400 символов (ограничение в `memory`/API, не в БД) |
| `source` | TEXT '' | `user`, `task:<id>`, `mcp:<agent>`, источник `memory.Access` |
| `pinned` | INTEGER 0 | закреплённые Оркестратор видит всегда |
| `created` | INTEGER NOT NULL | мс |
| `agent` | TEXT '' | id агента команды — личная память; `''` — общая |

Индекс `project_memory_project ON project_memory(project, id)` (на `agent` индекса нет).
Методы: `AddNote` (`memory.go:43`), `Notes(project)` (`:56`, новые первыми, включая личные заметки агентов),
`SetNotePinned(project, id, pinned)` (`:75`), `DeleteNote(project, id)` (`:86`). Тип `Note` (`:31`).
Пишут: tasks `learn` (`internal/tasks/tasks.go:520, 523` — итог/урок задачи, дубль в личную память агента),
`memory.Access` (`internal/memory/memory.go:261` — инструмент агента/MCP), API `/api/memory`,
`DeleteTeamAgent` (удаляет личные заметки).
Читают: `memory` (`memory.go:222`), tasks (`tasks.go:470`), semantic (`semantic.go:262` — индексирует заметки),
API (`memoryapi.go:28`, `teamapi.go:65`, `projects.go:268`).

### 4.11. `team` — постоянные агенты команды

`team.go:15-22`: `id` TEXT PK (`agent_[a-z0-9]{6,32}`), `project` TEXT NOT NULL, `data` TEXT NOT NULL
(**JSON всего `TeamAgent`**), `created` INTEGER (мс). Индекс `team_project ON team(project)`.

`TeamAgent` (`team.go:25-38`): `id, project, name, emoji, role (≤4000), scope[], model, fallback, skills[], mcp[], enabled, created`.
`Validate` (`team.go:43`): id по `agentIDRe` (`:40`), имя 1–40 рун, модель с `/`, роль ≤ 4000 рун.
Методы: `SaveTeamAgent` (`:57`; валидирует; upsert, при конфликте меняется только `data` — `project`
и `created` фиксированы), `TeamAgent(id)` (`:70`), `Team(project)` (`:83`, по `created`; битый JSON
пропускается молча), `DeleteTeamAgent(id)` (`:103`; затем `DELETE FROM project_memory WHERE agent = id`
без проверки ошибки).
Пишут/читают: API `teamapi.go`, `projects.go:508`, `memoryapi.go:86`; tasks через интерфейс `Team`
(`internal/tasks/tasks.go:175, 498`).

### 4.12. `mcp_servers` — каталог внешних MCP-серверов (конфиг зашифрован)

`mcpservers.go:17-25` + миграция 5.

| Колонка | Тип | Значение |
|---|---|---|
| `id` | INTEGER PK | |
| `name` | TEXT NOT NULL **UNIQUE** | `^[a-z0-9][a-z0-9_-]{0,31}$`, не `edy` |
| `description` | TEXT '' | для Оркестратора: когда выдавать сервер |
| `enabled` | INTEGER 1 | |
| `config` | **BLOB** NOT NULL | `vault.Seal(json(MCPConfig))` — тип, URL, заголовки, команда, аргументы, env |
| `account` | TEXT '' | подпись аккаунта («GitHub: работа») |

`MCPConfig` (`mcpservers.go:42-49`): `{type: "http"|"stdio", url?, headers?: map, command?, args?: [], env?: map}`
(`MCPHTTP`/`MCPStdio` — `:12-15`). Зашифрован **целиком**, включая URL и команду.
Методы: `Validate` (`:54`), `SaveMCPServer` (`:77`; upsert по `name`, перезаписывает всё, включая `enabled`),
`MCPServers()` (`:93`; расшифровывает все; ошибка расшифровки любой строки — ошибка всего списка),
`SetMCPServerEnabled` (`:119`), `DeleteMCPServer` (`:130`).
Пишут: API `mcpcatalog.go:125, 159, 170`. Читают: API (`mcpcatalog.go:76`, `projects.go:385`),
OmniRoute `climcp.go:115` (собирает конфиг MCP для Claude Code по именам из заголовка `X-Edy-MCP-Servers`).

### 4.13. `embeddings` — векторы поиска по смыслу

`embeddings.go:10-20`.

| Колонка | Тип | Значение |
|---|---|---|
| `project` | TEXT NOT NULL | путь |
| `key` | TEXT NOT NULL | `"<путь>#<начальная строка>"` для куска кода (`chunkKey`, `internal/semantic/semantic.go:483`) или `"note:<id>"` для заметки (`semantic.go:265`) |
| `hash` | TEXT NOT NULL | хеш текста куска — если не изменился, вектор переиспользуется |
| `model` | TEXT NOT NULL | модель эмбеддингов `провайдер/модель` |
| `vec` | BLOB NOT NULL | нормированный вектор float32, little-endian, 4 байта на компоненту (`encodeVec`/`decodeVec`, `:30`, `:38`) |
| `updated` | INTEGER NOT NULL | мс |

**PRIMARY KEY (project, key)**. Тип `Embedding` (`:23`).
Методы: `Embeddings(project, model)` (`:47`), `SaveEmbeddings(project, model, list, keep)` (`:68`) — в транзакции
upsert всех векторов; если `keep != nil` — удаляет ключи, которых нет в `keep`, **и** векторы другой модели.
Пишет/читает только `internal/semantic` (`semantic.go:268, 329, 405`) через интерфейс `semantic.Store`
(`semantic.go:114-120`).

### 4.14. Связи

```
providers 1──* api_keys            (FK, ON DELETE CASCADE)
agents    1──* agent_events        (FK, ON DELETE CASCADE)
board_tasks 1──* board_events      (FK, ON DELETE CASCADE)
board_tasks.parent → board_tasks.id          (логическая, без FK)
board_tasks.data.agent / agents → team.id    (логическая)
agents.task_id → board_tasks.id              (логическая)
project_memory.agent → team.id               (логическая; чистится в DeleteTeamAgent)
settings["project:<path>"].account_id → git_accounts.id   (логическая; не чистится)
все project-колонки → settings["projects"]   (логическая; не чистится при удалении проекта)
```

---

## 5. Ключи настроек (`settings`)

Полный список (найден поиском `Setting(` / `SetSetting(` по всему репозиторию):

| Ключ | Константа | Формат значения | Пишут | Читают |
|---|---|---|---|---|
| `router_token` | `settingRouterToken`, `internal/app/app.go:31` | `edy-` + 48 hex (`omniroute.NewToken`) | `app.EnsureToken` (`app.go:68`) — один раз при первом запуске | `app.EnsureToken` (`app.go:63`); копируется в `<Home>/router.token` |
| `daily_budget_usd` | `omniroute.SettingDailyBudget`, `internal/omniroute/server.go:28` | десятичное число строкой (`"5"`, `"2.5"`); `≤0`/не число — без лимита | API `PUT /api/budget` (`internal/api/api.go:710`), CLI `edy budget set` (`cmd/edy/main.go:113`) | OmniRoute `checkBudget` (`server.go:228`), API `/api/usage` (`api.go:696`) |
| `notify_off` | `api.SettingNotifyOff`, `internal/api/api.go:274` | `"1"` — уведомления выключены, `"0"`/нет — включены | `PUT /api/prefs` (`api.go:294`) | `GET /api/prefs` (`api.go:277`), `Tasks.Notify` в `internal/app/app.go:141` |
| `projects` | `settingProjects`, `internal/api/projects.go:27` | JSON-массив абсолютных путей, ≤ 10, последний добавленный первым | `saveProjectPaths` (`projects.go:81`): добавление/удаление проекта, `git clone` | `projectPaths` (`projects.go:70`), т. е. `knownProject` во всех маршрутах |
| `task_templates` | `settingTemplates`, `internal/api/projects.go:671` | JSON-массив строк, ≤ 50, каждая ≤ 1000 рун | `addTemplate`/`deleteTemplate` (`projects.go:713, 729`) | `customTemplates` (`projects.go:674`) |
| `embed_model` | `semantic.SettingModel`, `internal/semantic/semantic.go:33` | `провайдер/модель` или `""` (поиск по смыслу выключен) | `Engine.SetModel` (`semantic.go:169`) ← `PUT /api/semantic` | `Engine.Model` (`semantic.go:158`) |
| `micro.model` | `api.SettingMicroModel`, `internal/api/micro.go:15` | `провайдер/модель` или `""` (самая дешёвая) | `PUT /api/micro/settings` | `microModel` (`micro.go`) |
| `ui.prefs` | `api.SettingUIPrefs`, `internal/api/uiprefs.go:13` | JSON `UIPrefs` | `PUT /api/ui/prefs`, `POST /api/ui/bot` | `GET /api/ui/prefs` |
| `project:<абсолютный путь>` | — (`internal/store/git.go:134, 152`) | JSON `ProjectSettings` | `SetProjectSettings` ← `PUT /api/git/settings`, `POST /api/git/clone` (`gitapi.go:163, 240`) | `ProjectSettings` ← API (`gitapi.go:178, 263, 292`; `projects.go:265`), `internal/app/app.go:131, 149, 153` (ревьюер, MaxParallel, AutoApprove) |

### 5.1. `ProjectSettings`

`git.go:111-126`, JSON-теги:

| Поле JSON | Тип | По умолчанию | Значение |
|---|---|---|---|
| `commit` | string | `branch` | `branch` — коммит в новую ветку `edy/…`; `direct` — в текущую; `none` — без коммита (`git.go:97-101`) |
| `push` | string | `off` | `off`, `push`, `pr` — push + Pull/Merge Request (`git.go:104-108`) |
| `account_id` | int64 | 0 | аккаунт git для push/PR/pull (0 — системный git) |
| `author_name`, `author_email` | string | `""` | свой автор коммитов (оба или ни одного) |
| `prefix` | string | `"Edy: "` | префикс заголовка коммита |
| `auto_approve` | bool | false | принимать результат без человека, если проверки и ревью прошли |
| `review_model` | string | `""` | `""` — авто (резервная модель задачи, `review.AutoModel`), `off` — без ревью, иначе `провайдер/модель` |
| `max_parallel` | int | 0 | сколько агентов эпика одновременно (0 → 2; API разрешает 0–6) |

`DefaultProjectSettings()` (`git.go:129`) — `{Commit: branch, Push: off, Prefix: "Edy: "}`.
`ProjectSettings(path)` (`git.go:133`): нет ключа → умолчания; есть → JSON поверх умолчаний (поэтому
отсутствующие в старом JSON поля получают значения по умолчанию). Ключ — путь **как есть**, без нормализации.

---

## 6. Шифрование (vault)

Пакет `internal/vault` (`vault.go`): AES-256-GCM, мастер-ключ из Keychain (раздел 2.4).
`Seal(plaintext)` → `nonce (12 байт, crypto/rand) ‖ ciphertext+tag`; `Open(sealed)` проверяет тег —
подмена данных даёт ошибку. Дополнительных данных (AAD) нет: зашифрованное значение технически можно
переставить между строками, это осознанное упрощение.

Зашифрованные колонки (все типа BLOB):

| Таблица.колонка | Что внутри | Шифрует | Расшифровывает |
|---|---|---|---|
| `api_keys.secret` | API-ключ провайдера | `AddKey` (`store.go:248`) | `Keys` (`store.go:273`) |
| `git_accounts.token` | токен доступа хостинга | `AddGitAccount` (`git.go:39`) | `GitAccount` (`git.go:77`) |
| `mcp_servers.config` | JSON `MCPConfig` (URL, заголовки, команда, аргументы, env) | `SaveMCPServer` (`mcpservers.go:82`) | `MCPServers` (`mcpservers.go:106`) |

**Не** шифруются: `router_token` в `settings` (и файл `router.token`), `providers.base_url`, имена/описания
MCP-серверов, профили браузеров веб-чатов (`web-profiles/`, это обычные папки Chromium), содержимое
задач, память, журнал. Секреты не попадают в API-ответы: `KeyInfos` без секрета, `GitAccount.Token` с
`json:"-"`, у MCP-серверов наружу отдаются только имена заголовков/переменных.

---

## 7. Экспортированные типы, константы и методы

### 7.1. `store.go`

| Имя | Строка | Описание |
|---|---|---|
| `KindOpenAI`, `KindAnthropic`, `KindCLI`, `KindWeb` | 18-23 | типы провайдеров |
| `ErrNotFound`, `ErrExists` | 25-28 | ошибки-сигналы |
| `type Provider` | 75 | провайдер: `ID, Name, Kind, BaseURL, Models, PriceIn, PriceOut, Command, Site, Browser, Caps` (без JSON-тегов) |
| `func (Provider) Cost(in, out int64) float64` | 95 | стоимость запроса в USD |
| `type Key` | 100 | расшифрованный ключ `ID, Label, Secret` |
| `type Usage` | 107 | запись расхода для `RecordUsage` |
| `type Store` | 116 | обёртка над БД и vault |
| `func Open(path string, v *vault.Vault) (*Store, error)` | 122 | открыть/создать базу, схема, миграции |
| `(*Store) Close() error` | 140 | закрыть |
| `(*Store) AddProvider(p Provider) (int64, error)` | 142 | добавить провайдера; `ErrExists` при дубле |
| `(*Store) SetModels(name string, models []string) error` | 159 | заменить список моделей |
| `(*Store) SetCaps(name string, caps []string) error` | 171 | теги вручную; `nil` — автоопределение |
| `(*Store) DeleteProvider(name string) error` | 188 | удалить с ключами |
| `(*Store) Provider(name string) (Provider, error)` | 199 | по имени |
| `(*Store) Providers() ([]Provider, error)` | 208 | все, по имени |
| `(*Store) AddKey(providerID int64, label, secret string) (int64, error)` | 247 | зашифровать и сохранить ключ |
| `(*Store) Keys(providerID int64) ([]Key, error)` | 260 | активные ключи с расшифровкой |
| `type KeyInfo` | 284 | ключ без секрета (`id, label, disabled`) |
| `(*Store) KeyInfos(providerID int64) ([]KeyInfo, error)` | 291 | все ключи без секретов |
| `(*Store) DeleteKey(id int64) error` | 308 | удалить ключ |
| `(*Store) DisableKey(id int64) error` | 320 | выключить ключ |
| `(*Store) Setting(key string) (string, error)` | 325 | прочитать настройку |
| `(*Store) SetSetting(key, value string) error` | 334 | записать настройку (upsert) |
| `(*Store) RecordUsage(u Usage) error` | 339 | записать расход |
| `type UsageSummary` | 346 | сводка расходов по client/provider/model |
| `(*Store) UsageSince(since time.Time) ([]UsageSummary, error)` | 357 | сводка с момента |
| `(*Store) SpentSince(since time.Time) (float64, error)` | 376 | сумма USD с момента |

### 7.2. `agents.go`

| Имя | Строка | Описание |
|---|---|---|
| `RoleOrchestrator`, `RoleExecutor`, `RoleMCP`, `RoleReviewer` | 11-16 | роли |
| `AgentRunning`, `AgentDone`, `AgentFailed`, `AgentCancelled`, `AgentInterrupted` | 19-25 | состояния |
| `type Agent` | 53 | запись журнала (+ `Events` — число событий) |
| `type AgentEvent` | 69 | событие `id, at, kind, text` |
| `(*Store) CreateAgent(a Agent) error` | 76 | создать агента |
| `(*Store) FinishAgent(id, state, summary, errText string) error` | 89 | записать итог |
| `(*Store) TouchAgent(id, state, summary string) error` | 98 | обновить состояние «урывочного» агента |
| `(*Store) AddAgentEvent(agentID, kind, text string, at time.Time) error` | 107 | добавить событие |
| `(*Store) InterruptRunningAgents() (int64, error)` | 113 | пометить оставшихся `running` как `interrupted` |
| `(*Store) Agents(limit int) ([]Agent, error)` | 139 | последние агенты |
| `(*Store) Agent(id string) (Agent, error)` | 156 | агент по id |
| `(*Store) AgentEvents(agentID string, afterID int64) ([]AgentEvent, error)` | 165 | события после курсора |
| `type ModelStat` | 185 | `model, attempts, successes, cost_usd, tokens` |
| `(*Store) ModelStats(since time.Time) ([]ModelStat, error)` | 195 | успехи исполнителей (`agents`: role=executor, state≠running, success = done) + расходы (`usage`), по убыванию attempts |

### 7.3. `git.go`

| Имя | Строка | Описание |
|---|---|---|
| `GitHub`, `GitLab`, `Gitea` | 10-14 | виды хостингов |
| `type GitAccount` | 29 | аккаунт (`Token` с `json:"-"`) |
| `(*Store) AddGitAccount(a GitAccount) (int64, error)` | 38 | сохранить с шифрованием токена |
| `(*Store) GitAccounts() ([]GitAccount, error)` | 50 | список без токенов |
| `(*Store) GitAccount(id int64) (GitAccount, error)` | 68 | с расшифрованным токеном |
| `(*Store) DeleteGitAccount(id int64) error` | 85 | удалить |
| `CommitBranch`, `CommitDirect`, `CommitNone` | 97-101 | режимы коммита |
| `PushOff`, `PushOn`, `PushPR` | 104-108 | действие после коммита |
| `type ProjectSettings` | 111 | настройки проекта (раздел 5.1) |
| `func DefaultProjectSettings() ProjectSettings` | 129 | умолчания |
| `(*Store) ProjectSettings(path string) (ProjectSettings, error)` | 133 | прочитать `project:<path>` |
| `(*Store) SetProjectSettings(path string, ps ProjectSettings) error` | 147 | записать |

### 7.4. `board.go`

| Имя | Строка | Описание |
|---|---|---|
| `type BoardTask` | 38 | карточка, `Data json.RawMessage` |
| `type BoardEvent` | 51 | событие истории |
| `(*Store) SaveBoardTask(t BoardTask) error` | 59 | создать/обновить |
| `(*Store) BoardTask(id string) (BoardTask, error)` | 87 | по id |
| `(*Store) BoardTasks(project string) ([]BoardTask, error)` | 96 | карточки проекта (или все) |
| `(*Store) DeleteBoardTask(id string) error` | 119 | удалить (с событиями) |
| `(*Store) AddBoardEvent(taskID, kind, text string, at time.Time) error` | 130 | событие |
| `(*Store) BoardEvents(taskID string, afterID int64) ([]BoardEvent, error)` | 136 | события после курсора |

### 7.5. `memory.go`

| Имя | Строка | Описание |
|---|---|---|
| `NoteFact`, `NoteLesson`, `NoteTask`, `NoteNote` | 23-28 | виды заметок |
| `type Note` | 31 | заметка |
| `(*Store) AddNote(n Note) (int64, error)` | 43 | добавить |
| `(*Store) Notes(project string) ([]Note, error)` | 56 | все заметки проекта, новые первыми |
| `(*Store) SetNotePinned(project string, id int64, pinned bool) error` | 75 | закрепить/открепить |
| `(*Store) DeleteNote(project string, id int64) error` | 86 | удалить |

### 7.6. `team.go`

| Имя | Строка | Описание |
|---|---|---|
| `type TeamAgent` | 25 | постоянный агент |
| `(TeamAgent) Validate() error` | 43 | проверка перед сохранением |
| `(*Store) SaveTeamAgent(a TeamAgent) error` | 57 | создать/обновить |
| `(*Store) TeamAgent(id string) (TeamAgent, error)` | 70 | по id |
| `(*Store) Team(project string) ([]TeamAgent, error)` | 83 | команда проекта |
| `(*Store) DeleteTeamAgent(id string) error` | 103 | удалить с личной памятью |

### 7.7. `mcpservers.go`

| Имя | Строка | Описание |
|---|---|---|
| `MCPHTTP`, `MCPStdio` | 12-15 | виды подключения |
| `type MCPServer` | 31 | сервер каталога (встраивает `MCPConfig`) |
| `type MCPConfig` | 42 | как подключаться |
| `(MCPServer) Validate() error` | 54 | проверка имени, типа, URL/команды |
| `(*Store) SaveMCPServer(m MCPServer) error` | 77 | upsert с шифрованием конфига |
| `(*Store) MCPServers() ([]MCPServer, error)` | 93 | каталог с расшифровкой |
| `(*Store) SetMCPServerEnabled(name string, enabled bool) error` | 119 | включить/выключить |
| `(*Store) DeleteMCPServer(name string) error` | 130 | удалить |

### 7.8. `embeddings.go`

| Имя | Строка | Описание |
|---|---|---|
| `type Embedding` | 23 | `Key, Hash, Model, Vec []float32` |
| `(*Store) Embeddings(project, model string) ([]Embedding, error)` | 47 | векторы проекта для модели |
| `(*Store) SaveEmbeddings(project, model string, list []Embedding, keep map[string]bool) error` | 68 | сохранить и вычистить устаревшие |

---

## 8. Интерфейсы-потребители

Чтобы не тянуть `*store.Store` во все пакеты, потребители объявляют узкие интерфейсы (удобно для тестов):

| Интерфейс | Где | Методы |
|---|---|---|
| `tasks.Journal` | `internal/tasks/tasks.go:27` | `CreateAgent`, `AddAgentEvent`, `FinishAgent` |
| `tasks.Board` | `internal/tasks/tasks.go:34` | `SaveBoardTask`, `BoardTask`, `BoardTasks`, `DeleteBoardTask`, `AddBoardEvent`, `BoardEvents` |
| `tasks.Manager.Team` | `internal/tasks/tasks.go:175` | `TeamAgent` |
| `memory.Store` | `internal/memory/memory.go:23` | `AddNote`, `Notes` |
| `semantic.Store` | `internal/semantic/semantic.go:114` | `Embeddings`, `SaveEmbeddings`, `Notes`, `Setting`, `SetSetting` |

Связывание — в `internal/app/app.go:115-121`: `tasks.NewManager(llm, st)` (Board), `Journal = st`,
`Notes = st`, `Team = st`, `semantic.New(client, st)`.

---

## 9. Конкурентность и транзакции

* `*sql.DB` потокобезопасен; WAL + `busy_timeout(5000)` позволяют одновременную работу HTTP-обработчиков,
  горутин задач и индексации.
* Транзакция используется только в `SaveEmbeddings`. Остальные операции — одиночные `Exec`.
  Атомарность «прочитать-изменить-записать» карточек задач обеспечивает мьютекс `tasks.Manager.mu`
  (`update`, `internal/tasks/tasks.go`), а не БД.
* `DeleteTeamAgent` выполняет два независимых `DELETE` (без транзакции).

---

## 10. Тесты

* `store_test.go`: `TestProvidersAndEncryptedKeys` (27), `TestCommandRoundTripAndReopen` (67),
  `TestDuplicateAndDeleteProvider` (93), `TestUsageAndSettings` (113), `TestKeyInfosDeleteKeyAndUsageSince` (131),
  `TestMCPServersAreEncrypted` (160), `TestModelStats` (203).
* `agents_test.go`: `TestAgentJournal` (9), `TestGitAccountsAndProjectSettings` (53).
* Тесты создают базу во временной папке и `vault.New` с фиксированным ключом (без Keychain); `vault`
  в тестах Keychain подменяется `keyring.MockInit()`.

Запуск: `go test ./internal/store/...`.

---

## 11. Как посмотреть базу вручную

```sh
sqlite3 "$HOME/Library/Application Support/Edy/edy.db"
.tables
SELECT key, substr(value,1,80) FROM settings;      -- router_token здесь открытым текстом!
SELECT name, kind, models, caps FROM providers;
SELECT id, role, model, state, datetime(created/1000,'unixepoch') FROM agents ORDER BY created DESC LIMIT 20;
SELECT id, state, title FROM board_tasks WHERE project = '/path/to/project' ORDER BY position;
```

Зашифрованные BLOB-колонки без мастер-ключа из Keychain не прочитать. Редактировать базу при запущенном
Edy можно (WAL), но изменения `data` карточек в обход `tasks.Manager` рискованны.

---

## 12. Странности и долги

1. Новые колонки `mcp_servers.account` и `project_memory.agent` есть только в миграциях, не в `CREATE`;
   миграции выполняются при каждом старте и опираются на текст ошибки `duplicate column` (зависит от
   драйвера). Версии схемы нет.
2. Таблицы `agents`, `agent_events`, `usage`, `board_*` не чистятся никогда — база растёт.
3. Удаление проекта из списка не удаляет его данные (`project:<path>`, доска, память, команда, эмбеддинги).
4. Удаление git-аккаунта не обнуляет `account_id` в настройках проектов.
5. Выключенный ключ (`api_keys.disabled = 1`) нельзя включить обратно — нет метода.
6. `providers.models` — строка через запятую (модель с запятой невозможна).
7. `Team()` молча пропускает строки с битым JSON; `scanProvider` в той же ситуации (`command`) возвращает ошибку
   и ломает весь список провайдеров.
8. `router_token` хранится открытым текстом (в `settings` и в `router.token`) — это допустимо, так как он даёт
   доступ только к локальному Edy, но при утечке файла базы токен тоже утечёт.
9. Vault без AAD: зашифрованное значение можно переставить в другую строку той же колонки (требует доступа к файлу).
