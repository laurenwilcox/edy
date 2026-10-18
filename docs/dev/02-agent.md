# 02. Агент и Context Engine

Документ описывает ядро Edy, которое собственно «пишет код»: агентный цикл, инструменты агента,
хуки (линтеры/тесты), теневое версионирование (Shadow Git), путь через MCP для CLI-исполнителей
(Claude Code), а также Context Engine — скелет проекта, поиск кода BM25, поиск по смыслу,
память проекта и скиллы.

Пакеты, о которых идёт речь:

| Пакет | Файлы | Назначение |
|---|---|---|
| `internal/agent` | `task.go`, `workspace.go`, `hooks.go`, `shadow.go`, `llm.go` | Агентный цикл, инструменты, хуки, Shadow Git, клиент модели, git-операции Approve/Reject |
| `internal/codeindex` | `codeindex.go`, `search.go`, `pyskeleton.py` | Скелет проекта (Go через `go/ast`, Python через встроенный скрипт), нарезка на куски, BM25 |
| `internal/semantic` | `semantic.go` | Эмбеддинги через OmniRoute, инкрементальный индекс в SQLite, гибридный поиск (RRF) |
| `internal/memory` | `memory.go` | Память проекта: BM25 по заметкам с грубым стеммингом, выжимки для промптов, доступ агента с лимитом |
| `internal/skills` | `skills.go` | Скиллы в формате Claude Code (`<имя>/SKILL.md`): загрузка, сохранение, удаление, каталог для промпта |

Все тексты промптов, ошибок и событий — на русском. Это часть продукта (интерфейс и агенты русскоязычные),
поэтому при правках сохраняйте язык.

Ссылки вида `файл:строка` указаны на состояние кода, описанное в [00-README.md](00-README.md).

---

## 1. Назначение файлов

### 1.1 `internal/agent`

| Файл | Что внутри |
|---|---|
| `llm.go` | Комментарий пакета (`llm.go:1-3`). Формат сообщений OpenAI (`Msg`, `ToolCall`, `ToolDef`), интерфейс `LLM`, HTTP-клиент `OmniRoute` к локальному прокси, `CallOptions` (рабочая папка и MCP для CLI-провайдеров, передаются заголовками), `InProcess` — HTTP-клиент, вызывающий обработчик в том же процессе без сети. |
| `task.go` | `Task` и его `Run` — главный цикл с цепочкой моделей и Circuit Breaker; `attempt` (модель с инструментами в промпте) и `attemptMCP` (CLI-исполнитель с MCP Edy); системные промпты; события `Event`; итог `Result`; git-операции над **основным** репозиторием проекта: `CheckClean`, `Branch`, `BranchName`, `HasChanges`, `FreeBranch`, `Approve`, `Reject`, `excludeEdyDir`, транслитерация для имени ветки. |
| `workspace.go` | `Workspace` — «песочница» папки проекта: описания инструментов (`Tools`), их выполнение (`Call`), защита путей (`resolve`), вспомогательные `Files`, `ReadText` для интерфейса. Интерфейсы `CodeSearch` и `Memory`, которые реализуют `semantic.Code` и `memory.Access`. |
| `hooks.go` | `Hook`, `DetectHooks` (из `.edy/hooks.json` или по стеку проекта), `RunHooks` (последовательный запуск с таймаутом и обрезкой вывода), `lastRunes`. |
| `shadow.go` | `Shadow` — теневой git-репозиторий `.edy/shadow.git`: `Init`, `Snapshot`, `Restore`, `RestoreFile`, `List`, `Diff`, `Patch`, `OpenShadow`. |
| `agent_test.go` | Сценарные тесты цикла с фальшивой моделью `scripted`. |
| `integration_test.go` | Сквозной тест через настоящий OmniRoute и «текстовую» CLI-утилиту (эмуляция tool calling). |

### 1.2 Context Engine

| Файл | Что внутри |
|---|---|
| `internal/codeindex/codeindex.go` | `Build` (обход проекта), разбор Go (`parseGo`, `typeSummary`), Python (`parsePython` → `pyskeleton.py` или регулярное выражение), `Skeleton`. |
| `internal/codeindex/search.go` | `Chunk`, `Hit`, токенизатор с разбиением идентификаторов, `Chunks`, BM25 `Search`, `FormatHits`. |
| `internal/codeindex/pyskeleton.py` | Встраивается через `//go:embed`; читает пути из stdin, печатает JSON со списком `def`/`class` (включая вложенные методы как `Class.method`). |
| `internal/semantic/semantic.go` | `Client` (эмбеддинги OmniRoute), `Engine` (индекс, статус, кеш векторов), `Fuse` (RRF), адаптеры `Code` (реализует `agent.CodeSearch`) и `Notes` (реализует `memory.Ranker`). |
| `internal/memory/memory.go` | `Search` (BM25), `Brief`, `Pinned`, `AgentBrief`, `Access` (инструменты `project_memory` и `remember`), `Ranker`, внутренний `fuse`. |
| `internal/skills/skills.go` | `Skill`, `Dir`, `Dirs`, `Load`, `Find`, `Save`, `Remove`, `Catalog`, `ValidName`, разбор front matter. |

---

## 2. Агентный цикл

### 2.1 Кто запускает агента

* `internal/tasks/tasks.go:998` — `Manager.execute` собирает `agent.Task` для задачи с доски
  (память, поиск кода, скиллы, роль постоянного агента, скриншоты, MCP-мост, внешние MCP-серверы)
  и вызывает `task.Run(ctx)` (`tasks.go:1093`). Это основной путь.
* `cmd/edy/main.go:338` — CLI-команда `edy task`: тот же `Task`, события печатаются в терминал
  с иконками, после `Done` спрашивает «Применить?» → `Approve` или `Reject`.

### 2.2 Поля `Task` (`task.go:34`)

| Поле | Смысл |
|---|---|
| `Root` | Папка проекта (или копия-worktree для подзадачи эпика). |
| `Goal` | Текст задачи. Первая строка идёт в сообщения снимков и имя ветки. |
| `Models` | Цепочка «провайдер/модель». Порядок важен: от дешёвой к сильной. |
| `LLM` | Реализация `LLM` (в проде — `*OmniRoute` с `InProcess`). |
| `MaxAttempts` | Провалов подряд на одну модель до передачи следующей. `<=0` → 3 (`task.go:122`). |
| `MaxSteps` | Ответов модели на одну модель (на весь её `attempt`, а не на одну попытку хуков!). `<=0` → 40 (`task.go:125`). |
| `OnEvent` | Колбэк ленты событий. |
| `OnAttempt` / `OnAttemptEnd` | Начало/конец работы очередной модели (в tasks — отдельный «агент» в журнале). |
| `Skills` | Скиллы, доступные агенту (`use_skill`). |
| `MCP` | `MCPBridge`; `nil` — все модели идут через `attempt`. |
| `Role` | Инструкции постоянного агента — дописываются в системный промпт. |
| `Memory`, `MemoryBrief` | Доступ к памяти и закреплённая выжимка в промпте. |
| `Code` | Поиск по смыслу для `search_code`; `nil` — только BM25. |
| `Images`, `CanSee` | Скриншоты (data:-URL) и функция «видит ли модель картинки». |
| `Extra` | Имена внешних MCP-серверов из каталога (только для MCP-исполнителей). |

### 2.3 `Run` по шагам (`task.go:121-221`)

```
Run(ctx)
 │
 ├─ defaults: MaxAttempts=3, MaxSteps=40; нет моделей → StatusFailed + ошибка
 ├─ CheckClean(root)            git есть? папка есть? если репо: есть HEAD, .edy в exclude, status пуст
 ├─ isRepo ? excludeEdyDir : emit info "Папка без git…"
 ├─ DetectHooks(root)           emit info "Проверки после работы: go vet, go test" / "Проверки не найдены…"
 ├─ shadow.Init ; baseline = shadow.Snapshot("Начало задачи: …")   emit checkpoint "Снимок №0"
 ├─ ctx = WithCallOptions(ctx, {Workdir: root})
 │
 └─ for model in Models:                                  ◄──────────────┐
      ├─ mi>0 → emit breaker "Circuit Breaker: проект откачен…"          │
      ├─ OnAttempt(model)                                                │
      ├─ MCP != nil && MCP.Supports(model) ?                             │
      │     attemptMCP(...)            : attempt(...)                    │
      ├─ OnAttemptEnd(model, ok && err==nil, note)                       │
      ├─ err != nil → return {Failed, Baseline}, err  (откат НЕ делается) │
      ├─ ok → final = Snapshot("Готово: …"); Diff(--stat от baseline)    │
      │       return {Done, Summary, Model, Baseline, Final, Diff}       │
      └─ !ok → shadow.Restore(baseline)  ─────────────────────────────────┘
   все модели провалились → emit breaker "Ни одна модель не справилась…" → {NeedsHuman, Baseline}
```

Важно:

* Каждая следующая модель начинает **с чистой историей** и с исходного состояния проекта
  (откат к `baseline`). Тест `TestCircuitBreakerEscalatesAndRollsBack` это проверяет: у второй модели
  первый запрос содержит ровно 2 сообщения.
* Счётчик снимков `step` общий для всех моделей (передаётся указателем) — номера «Снимок №N» растут
  сквозь всю задачу.
* При ошибке (`err != nil`, например отмена или сбой git) `Run` **не откатывает** проект. Это делает
  вызывающий: `tasks.Manager.work` (`tasks.go:551-557`) при `ctx.Err() != nil` вызывает
  `agent.Reject(baseline)`; при старте Edy незавершённые задачи тоже откатываются (`tasks.go:940-946`).
* При `StatusNeedsHuman` проект уже откачен (откат делается после каждой неуспешной модели, включая последнюю).
* При `StatusDone` правки остаются в рабочей папке, ждут Approve (коммит в новую ветку) или Reject.

### 2.4 `attempt` — модель с инструментами в промпте (`task.go:224-298`)

Начальные сообщения (`task.go:225-228`):

1. `system`: `systemPrompt` + `skills.Catalog(t.Skills)` + `memoryNote()` (роль + раздел о памяти).
2. `user`: `"Задача:\n" + Goal`, с картинками, если `CanSee(model)`.

Системный промпт целиком (`task.go:108-117`):

```
Ты — агент-разработчик в Edy. Ты работаешь в папке проекта и меняешь код только через инструменты.

Как работать:
1. Посмотри скелет проекта (project_skeleton): файлы, типы и функции с номерами строк.
   Нужный код ищи через search_code, файлы целиком читай (read_file) только когда это необходимо.
2. Внеси изменения: replace_in_file для точечных правок, write_file для новых файлов.
3. Когда задача выполнена, вызови finish с кратким итогом.

После finish Edy сам запустит проверки (линтеры и тесты). Если они упадут, ты получишь вывод и должен исправить ошибки.
Не трогай файлы, не связанные с задачей. Пиши код в стиле проекта. Отвечай по-русски.
```

`memoryNote()` (`task.go:314-327`) добавляет:

* `"\n\n" + Role`, если задан (постоянный агент);
* если `Memory != nil`: «Память проекта: ищи в ней через project_memory (как запускать, что хрупко, уроки прошлых задач); полезный неочевидный факт запиши через remember.»
  и, если `MemoryBrief` непуст, «Закреплённое в памяти проекта:\n» + выжимка.

Цикл:

```
loop:
  ctx отменён?                → return err
  steps >= MaxSteps?          → emit error "превышено N шагов без finish"; return (не ok)
  reply = LLM.Chat(model, msgs, tools)
     ошибка → emit error; failures++ ; failures>=MaxAttempts → return (не ok); continue  (без паузы!)
  msgs += reply ; текст → emit model (последние 600 символов)
  нет tool_calls → msgs += user "Используй инструменты… вызови finish."; continue
  для каждого tool_call:
     finish → finished=true, summary=args.summary, tool-ответ "Принято, запускаю проверки."
     иначе  → out,changed,err = ws.Call(name,args); err → "Ошибка: …"
              emit tool "<name> <path> → <первая строка результата, ≤200>"
              msgs += tool-ответ
              changed → step++; Snapshot("Шаг N: <tool> <path>"); emit checkpoint
  !finished → continue
  ok,report = RunHooks ; emit hooks report
  ok → return summary, true
  failures++ ; failures>=MaxAttempts → emit breaker "<model> провалила проверки N раза подряд"; return (не ok)
  msgs += user "Проверки не прошли (попытка i из N). Исправляй:\n\n" + report
```

Особенности, о которых легко забыть:

* `failures` общий для ошибок вызова модели и провалов хуков: три сетевые ошибки подряд «съедают» модель так же,
  как три упавших теста.
* Ошибка инструмента не прерывает цикл — модель получает текст `Ошибка: …` и пробует снова.
* Если в одном ответе несколько tool_calls, выполняются все, даже идущие после `finish`.
* Аргумент `path` в событиях берётся через `argString` (`task.go:418`) — у `search_code` и т.п. он пуст.
* Снимок делается только для инструментов, вернувших `changed=true` (`write_file`, `replace_in_file`, `delete_file`).

### 2.5 `attemptMCP` — CLI-исполнитель через MCP Edy (`task.go:343-416`)

Используется, когда `t.MCP != nil && t.MCP.Supports(model)`. В проде `Supports`
(`internal/api/mcpapi.go:222`) — провайдер типа CLI с тегом возможности `mcp` (`omniroute.CapMCP`),
на практике — Claude Code.

1. `t.MCP.Bind(root, MCPEvents{...})` (`mcpapi.go:231`) регистрирует привязку со случайным ключом и
   возвращает URL `http://<addr>/mcp?project=<root>&bind=<key>` и `release` (вызывается через `defer`).
   В `MCPEvents` передаются: имена скиллов, `Memory`, `Code`, колбэки `OnCall` (→ события `tool`/`error`)
   и `OnChange` (→ `step++` под мьютексом и событие `checkpoint` «Снимок №N — …»).
2. `ctx = WithCallOptions(ctx, {Workdir, MCP: url, MCPServers: Extra})`. `OmniRoute.Chat` превращает это
   в заголовки `X-Edy-Workdir`, `X-Edy-MCP`, `X-Edy-MCP-Servers` (`llm.go:115-125`). CLI-адаптер OmniRoute
   (`internal/omniroute/climcp.go:91-92`) запускает Claude Code с `--mcp-config … --strict-mcp-config
   --allowedTools <инструменты edy> --disallowedTools Bash Edit Write MultiEdit NotebookEdit` — то есть
   встроенная правка файлов и терминал у утилиты реально отключены.
3. Промпт — одно сообщение `user`: `mcpPrompt` + каталог скиллов + `memoryNote()` + (если `Extra`)
   абзац про выданные MCP-серверы «…правь файлы проекта только через edy» + «Задача:\n» + Goal.
   `tools` в `Chat` не передаются (`nil`): цикл ведёт сама утилита.

   `mcpPrompt` (`task.go:329-338`):
   ```
   Ты — агент-разработчик в Edy. Работай с проектом через инструменты MCP-сервера edy:
   project_skeleton и search_code — чтобы разобраться в коде, read_file — прочитать файл,
   replace_in_file и write_file — править, delete_file — удалять, run_checks — запустить линтеры и тесты,
   list_skills и use_skill — инструкции проекта. Встроенные инструменты правки файлов и терминал отключены.

   Как работать:
   1. Посмотри скелет проекта, найди нужный код.
   2. Внеси изменения. Не трогай файлы, не связанные с задачей. Пиши код в стиле проекта.
   3. Вызови run_checks и исправь ошибки, пока проверки не пройдут.
   4. В конце ответь кратким итогом: что сделано, 1–3 предложения. Отвечай по-русски.
   ```
4. Ответ утилиты (последние 600 символов) — это `summary`. Затем Edy **сам** запускает `RunHooks`.
   Прошли — успех. Нет — `failures++` и новый вызов с тем же промптом плюс
   «Предыдущая попытка уже внесла правки в проект, но проверки не прошли (попытка i из N). Исправь:» + отчёт.
   Правки предыдущей попытки не откатываются (в отличие от смены модели).
5. Ошибка вызова: если отменён `ctx` — возвращается `ctx.Err()`; иначе считается провалом.

Что происходит на стороне MCP-сервера (`internal/mcp/mcp.go`, подробнее — [04-mcp.md](04-mcp.md)):

* Список инструментов (`mcp.go:215`) — это `Workspace.Tools()` без `finish` плюс `run_checks` и `list_skills`;
  `use_skill` есть всегда. Аннотация `readOnlyHint` для читающих инструментов.
* Выполнение (`mcp.go:236`) — через тот же `agent.Workspace.Call`; пишущие инструменты сериализуются
  мьютексом сервера; после `changed` — снимок через `agent.OpenShadow` + `Snapshot("MCP: <tool> <path>")`
  (`mcp.go:291`) и `OnChange`.
* Для привязанного исполнителя (`bind != nil`) проверка `Busy` не ставится (`mcpapi.go:146-153`), так что
  правки разрешены, хотя задача «держит» проект. Внешним MCP-клиентам во время задачи правки запрещены.

### 2.6 Картинки (`task.go:301-311`, `llm.go:28-41`)

`imagesFor(model)`: нет картинок → `nil`; `CanSee == nil` или `false` → событие `info` «…не видит картинки —
работает только по тексту», `nil`; иначе событие `info` и `t.Images`. Картинки прикладываются к
первому `user`-сообщению; `Msg.MarshalJSON` превращает такое сообщение в массив частей
`[{type:text},{type:image_url,image_url:{url}}…]`. В `attemptMCP` `imagesFor` вызывается на **каждой**
итерации, поэтому событие повторяется при каждом перезапуске.

### 2.7 `Result` (`task.go:93-100`) и статусы (`task.go:20-24`)

| Поле | Значение |
|---|---|
| `Status` | `done` — хуки прошли, ждём Approve; `needs_human` — все модели исчерпаны, проект откачен; `failed` — ошибка самого Edy (git, отмена, нет моделей и т.п.). |
| `Summary` | Итог от `finish.summary` или последний текст MCP-исполнителя. |
| `Model` | Модель, которая справилась. |
| `Baseline` | id снимка до начала (заполняется даже при ошибке, если снимок успел сделаться). |
| `Final` | id снимка «Готово: …». |
| `Diff` | `git diff --cached --stat` от `baseline` — **только статистика**, не патч. Полный патч tasks получает отдельно через `Shadow.Patch` (`tasks.go:564`, лимит `patchLimit = 400 КБ`, `tasks.go:140`). |

### 2.8 События (`Event`, `task.go:27-31`)

| `Kind` | Когда | Пример текста |
|---|---|---|
| `info` | Служебное: нет git, список проверок, число скиллов, картинки, MCP, «Готово…» | `Проверки после работы: go vet, go test` |
| `checkpoint` | Каждый снимок | `Снимок №2 — replace_in_file calc.go` |
| `model` | Текст ответа модели (≤600 последних символов) | `cheap/m: Посмотрю код.` |
| `tool` | Вызов инструмента | `read_file calc.go → package calc` |
| `hooks` | Отчёт хуков | `✓ go vet\n✗ go test (go test ./...):\n…` |
| `breaker` | Circuit Breaker: передача модели, провал 3 раза, «Ни одна модель не справилась» | |
| `error` | Ошибка вызова модели, превышение шагов, ошибка MCP-инструмента | |

Потребители:

* `tasks.Manager.execute` (`tasks.go:1019-1031`) пишет каждое событие в ленту доски
  (`Board.AddBoardEvent`) и в журнал текущего агента (`Journal.AddAgentEvent`); события до первого
  `OnAttempt` буферизуются в `ru.pending`.
* `cmd/edy/main.go:337-346` печатает их в терминал с иконками (🤖 🔧 📸 🧪 ⚡ ℹ️ ⚠️).
* Интерфейс читает ленту доски/журнал через API (см. [03-orchestrator-board.md](03-orchestrator-board.md)).

### 2.9 Отмена

* Отмена задачи — `tasks.Manager.Cancel` (`tasks.go:799`) вызывает `cancel()` контекста.
* `attempt`/`attemptMCP` проверяют `ctx.Err()` в начале каждой итерации; `OmniRoute.Chat` прерывает HTTP-запрос.
* Хуки запускаются `exec.CommandContext` с контекстом задачи — тесты убиваются.
* Shadow Git **не** прерывается (см. §6.4) — снимок, начатый в момент отмены, доводится до конца.
* После возврата `Run` с ошибкой tasks откатывает проект к baseline.

### 2.10 Git-операции основного репозитория (`task.go:435-580`)

| Функция | Что делает |
|---|---|
| `CheckClean` (`:455`) | Требует `git` в PATH (подсказка `xcode-select --install`), существование папки. Для не-репо — ок. Для репо: есть коммит HEAD, `.edy/` добавлена в `.git/info/exclude`, `git status --porcelain` пуст. |
| `excludeEdyDir` (`:488`) | Дописывает `.edy/` в `<git-dir>/info/exclude` (не в `.gitignore` проекта), идемпотентно. Использует `context.Background()`. |
| `Branch` (`:483`) | Текущая ветка (`rev-parse --abbrev-ref HEAD`). |
| `BranchName` (`:510`) | `edy/<slug>-MMDD-HHMM`: первая строка задачи → транслит кириллицы → `[^a-z0-9]+`→`-` → ≤40 символов; пусто → `task`. Пример из теста: «Почини функцию сложения» → `edy/pochini-funkciyu-slozheniya-1017-2105`. |
| `HasChanges` (`:523`) | Есть ли незакоммиченные изменения; для папки без git всегда `true`. |
| `FreeBranch` (`:533`) | `name`, `name-2`, … до `name-99`, если ветка уже существует. |
| `Approve` (`:545`) | `check-ref-format --branch`, `checkout -q -b <branch>`, `add -A`, `commit -q -m`. Один коммит в новой ветке, проект переключается на неё. `.edy/` не попадает благодаря exclude. |
| `Reject` (`:560`) | `Shadow.Restore(baseline)`. |

---

## 3. Справочник инструментов агента

Описания и JSON-схемы — `Workspace.Tools()` (`workspace.go:94-123`), выполнение — `Workspace.Call`
(`workspace.go:126-241`). Аргументы разбираются в одну структуру с полями
`path, content, old, new, query, name, text, limit`; не-JSON аргументы → ошибка
«аргументы инструмента X — не JSON». Пустая строка аргументов допустима.

### 3.1 Сводная таблица

| Инструмент | Параметры (JSON) | Обязательные | Меняет файлы | Когда доступен |
|---|---|---|---|---|
| `project_skeleton` | `{}` | — | нет | всегда |
| `search_code` | `query: string`, `limit: integer` (по умолч. 5) | `query` | нет | всегда |
| `list_files` | `path: string` (папка, по умолч. корень) | — | нет | всегда |
| `read_file` | `path: string` | `path` | нет | всегда |
| `write_file` | `path: string`, `content: string` | оба | **да** | всегда |
| `replace_in_file` | `path`, `old`, `new` (строки) | все | **да** | всегда |
| `delete_file` | `path: string` | `path` | **да** | всегда |
| `finish` | `summary: string` («что сделано, 1-3 предложения») | `summary` | нет | всегда (только в `attempt`; в MCP исключён) |
| `project_memory` | `query: string` | `query` | нет | `Memory != nil` |
| `remember` | `text: string` (до 400 символов) | `text` | нет (пишет в БД) | `Memory != nil` |
| `use_skill` | `name: string` | `name` | нет | `len(Skills) > 0` |
| `run_checks` | `{}` | — | нет | только MCP-сервер |
| `list_skills` | `{}` | — | нет | только MCP-сервер |

Без памяти и скиллов модель видит ровно 8 инструментов (на это опирается `TestOmniRouteClient`).

Константы (`workspace.go:21-25`): `maxReadBytes = 200 KiB`, `maxListed = 500`, `skeletonLimit = 30000`.

### 3.2 Подробно

**`project_skeleton`** — `codeindex.Build(context.Background(), Root)` на каждый вызов (индекс не кешируется)
и `idx.Skeleton(30000)`. Формат — §8.1. Ошибки — только ошибки обхода папки.

**`search_code`** — тоже строит индекс заново. Пустой/пробельный `query` → «query не может быть пустым».
`limit <= 0 || limit > 20` → 5. Без `Code` — `codeindex.Search` (BM25); с `Code` — `Code.Search` (гибрид),
а непустое пояснение `note` выводится первой строкой в скобках, например
`(поиск по смыслу недоступен: …)`. Результат форматируется `FormatHits(hits, 80)`: заголовок
`── path · Symbol (строки A–B)` и до 80 строк кода с номерами, дальше «…ещё N строк — read_file».
Пустой результат: «Ничего не найдено. Попробуйте другие слова или посмотрите project_skeleton.»

**`list_files`** — рекурсивный обход от `resolve(path)`; пропускаются каталоги из `skipDirs` агента
(кроме стартового); пути относительно корня через `/`, отсортированы; максимум 500, затем
«…(показаны первые 500)». Пусто — «(файлов нет)». Ошибки обхода внутри молча игнорируются.

**`read_file`** — `os.ReadFile`; больше 200 КБ → первые 200 КБ + «\n…(файл обрезан)». Обрезка по байтам
может разрезать UTF-8 символ (в отличие от `ReadText`). Двоичные файлы не отсекаются. Ошибка:
«не удалось прочитать <path>: <причина>» — абсолютный путь вырезается `unwrapPath`.

**`write_file`** — создаёт родительские каталоги (`0755`), пишет файл `0644` целиком. Ответ
«записано <path> (<N> байт)». Ограничения на размер контента нет.

**`replace_in_file`** — читает файл, считает вхождения `old`:
пустой `old` → «old не может быть пустым»; 0 → «фрагмент old не найден в <path>»; >1 → «фрагмент old
встречается в <path> N раз — уточните его». Ровно 1 — замена и запись с правами `0644`
(права исходного файла не сохраняются — исполняемый скрипт потеряет `+x`). Ответ «заменено в <path>».

**`delete_file`** — `os.Remove` (только файл или пустая папка). Ответ «удалено <path>».

**`finish`** — обрабатывается в `attempt`, не в `Workspace.Call` (там он «неизвестный инструмент»).

**`project_memory`** — `Memory.Search(query)`: строки `- [факт] текст` (§9.1). Пустой результат —
«В памяти проекта ничего похожего не нашлось.» / «Память проекта пуста.»

**`remember`** — `Memory.Remember(text)`: пустая заметка, длиннее 400 символов, лимит заметок за задачу
(по умолчанию 5; у внешних MCP-клиентов 20) — ошибки. Ответ «записано в память проекта».

**`use_skill`** — `skills.Find(Skills, name)`; не найден — «скилл "x" не найден». Ответ
`# Скилл <name>\n\n<body>`.

**Неизвестное имя** — «неизвестный инструмент "x"».

---

## 4. Безопасность путей (`Workspace.resolve`, `workspace.go:55-91`)

Все файловые инструменты, `list_files`, `ReadText` и `Shadow.RestoreFile` проходят через `resolve`:

1. Пустой путь → `.` (корень).
2. Абсолютный путь запрещён: «путь "…" должен быть относительным (от корня проекта)».
3. `filepath.Clean`; если результат `..` или начинается с `../` — «выходит за пределы проекта».
4. Первый компонент пути `.git` или `.edy` — «папка X служебная, её трогать нельзя».
   Так защищены основной репозиторий, теневой репозиторий, `.edy/hooks.json` и скиллы проекта.
5. Корень проекта приводится через `EvalSymlinks` (на macOS `/var` → `/private/var`).
6. Проверка симлинков: от целевого пути вверх ищется первый существующий предок, он раскрывается
   `EvalSymlinks`, и результат обязан быть корнем или лежать внутри него. Иначе — «ведёт за пределы проекта».
   Так ловятся `link/secret`, где `link → /tmp/outside` (тест `TestWorkspaceConfinement`).

`skipDirs` агента (`workspace.go:28`): `.git`, `.edy`, `node_modules`, `vendor`, `__pycache__`, `.venv`, `venv`.
Они только скрываются из `list_files`/`Files`/поиска Python-тестов — **читать и писать в них можно**
(кроме `.git` и `.edy` в корне). У `codeindex` свой, более широкий список (§8.1).

Известные дыры (подробнее в §12):

* Проверка `.git`/`.edy` чувствительна к регистру, а файловая система macOS по умолчанию — нет:
  путь `.GIT/config` или `.Edy/shadow.git/HEAD` пройдёт проверку и попадёт в служебную папку.
* «Висячий» симлинк внутри проекта, указывающий наружу на несуществующий файл: `EvalSymlinks` для самого
  пути падает, проверяется родитель (он внутри), а `os.WriteFile` пойдёт по ссылке и создаст файл снаружи.
* Вложенные `.git` (подмодули, `sub/.git`) не защищены.

---

## 5. Хуки (`hooks.go`)

### 5.1 Определение проверок — `DetectHooks` (`hooks.go:29-53`)

1. Если есть `.edy/hooks.json` (`HooksFile`, `hooks.go:25`) — берётся **только** он:
   ```json
   [{"name": "make", "cmd": ["make", "check"]},
    {"name": "lint", "cmd": ["golangci-lint", "run"]}]
   ```
   Ошибка JSON → ошибка `Run` («.edy/hooks.json: …»), задача не стартует. Пустой массив `[]` = «без проверок».
   Команда запускается без shell: нужны пайпы — пишите `["sh", "-c", "…"]`.
2. Иначе по стеку:
   * `go.mod` в корне → `go vet ./...` и `go test ./...`;
   * `pyproject.toml` / `requirements.txt` / `setup.py` / любой `*.py` в корне →
     `ruff check .` (если `ruff` в PATH) и `pytest -q` (если `pytest` в PATH и найден хотя бы один
     `test_*.py` / `*_test.py`, `hasPythonTests`, `hooks.go:65`).
3. Ничего не найдено → пустой список; `Run` предупреждает «результат примется без тестов» и любая работа
   с `finish` сразу считается успешной.

Внимание: `.edy/` спрятана от git, поэтому `hooks.json` не попадает в репозиторий и живёт только на этой машине.
Агент изменить `hooks.json` не может (`resolve` запрещает `.edy`).

### 5.2 Запуск — `RunHooks` (`hooks.go:85-107`)

* Хуки идут **последовательно**, все до конца (даже после первого упавшего) — модель получает полный отчёт.
* Каждый — `exec.CommandContext` с таймаутом `hookTimeout = 10 минут` (`hooks.go:16`) поверх контекста задачи,
  `Dir = root`, stdout и stderr в один буфер.
* Успех: строка `✓ <name>`. Провал (ненулевой код, таймаут, не найдена команда):
  `✗ <name> (<cmd>):\n<последние 4000 символов вывода>`. Обрезка — `lastRunes` (`hooks.go:109`),
  по рунам, с префиксом `…`; хвост выбран потому, что в конце вывода go test/pytest — итог и упавшие тесты.
* Хук с пустым `cmd` пропускается.
* Переменные окружения наследуются от процесса Edy (важно для `PATH` в Edy.app — см. [08-app-build.md](08-app-build.md)).

---

## 6. Shadow Git (`shadow.go`)

### 6.1 Где живёт

Отдельный «голый» git-каталог `<project>/.edy/shadow.git` (`gitDir`, `shadow.go:29`) с рабочим деревом —
самой папкой проекта: все команды запускаются как
`git --git-dir=<root>/.edy/shadow.git --work-tree=<root> …` (`shadow.go:37`).
Основной `.git` проекта не трогается; в папке без git Shadow Git — единственная история.

Автор и коммиттер снимков — `Edy <edy@localhost>` (через переменные окружения, `shadow.go:40`),
`commit.gpgsign=false` ставится в конфиге теневого репо — чтобы снимки не требовали подписи пользователя.

> `docs/ARCHITECTURE.md` говорит про `.edy/shadow` и go-git. Фактически используется **git CLI** и
> `.edy/shadow.git`. Верить коду.

### 6.2 `Init` — идемпотентный (`shadow.go:53-70`)

`mkdir .edy` → `git init -q` (повторный init безопасен) → **каждый раз** перезаписывается
`shadow.git/info/exclude` содержимым `.edy/\n.git/\n` → `config commit.gpgsign false`.
Перезапись при каждом вызове — сознательная: если создание когда-то оборвали посередине, снимки
не начнут захватывать саму папку `.edy` (это раздувало бы репозиторий рекурсивно).
`.gitignore` проекта учитывается git'ом автоматически.

`OpenShadow(ctx, root)` (`shadow.go:124`) — то же плюс `excludeEdyDir` для основного репо; используется
MCP-сервером для правок вне задачи.

### 6.3 Снимки, baseline, final

* `Snapshot(msg)` (`:73`): `add -A` + `commit -q --allow-empty --no-verify -m msg` + `rev-parse HEAD`.
  `--allow-empty` — снимок есть всегда, даже если шаг ничего не изменил; `--no-verify` — хуки
  пользователя (если вдруг настроены глобально) не запускаются.
* Последовательность сообщений в задаче:
  `Начало задачи: <goal>` (baseline) → `Шаг N: <tool> <path>` / `MCP: <tool> <path>` → `Готово: <summary>` (final).
* `List` (`:97`): `git log --format=%H%x1f%cI%x1f%s` → `[]Checkpoint` (новые первыми).
* `Diff(from)` (`:115`): `add -A` + `diff --cached --stat from` — статистика для `Result.Diff`.
* `Patch(from, limit)` (`:135`): `add -A` + `diff --cached --no-color -U3 from`, обрезка до `limit` байт
  с припиской «… (изменения обрезаны: показаны первые N КБ)». Используется для ревью и интерфейса.

### 6.4 Почему `context.WithoutCancel`

`Shadow.git` (`shadow.go:31-47`) запускает каждую команду с `context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)`.
Если убить git посередине `add`/`commit`/`reset`, остаётся `index.lock` и неполные объекты — следующий
снимок и, главное, откат к baseline станут невозможны. Поэтому отмена задачи на git не действует;
команды короткие, а от зависания защищает 5-минутный таймаут.

### 6.5 Откаты

* `Restore(id)` (`:85`): проверка id (непустой, без пробелов и `-` — защита от инъекции опций),
  `reset -q --hard id` + `clean -q -fd`. Изменённые файлы восстанавливаются, созданные после снимка —
  удаляются. **Игнорируемые** (`.gitignore`, exclude) не трогаются: `clean` без `-x`.
* `Reject(root, baseline)` (`task.go:560`) = `Shadow{root}.Restore(baseline)`.
* `RestoreFile(id, file)` (`:151`): проверка id и `resolve(file)`; если в снимке файл был
  (`cat-file -e id:file`) — `checkout id -- file`; если не было — файл создан задачей и удаляется.
  Используется для отката одного файла из интерфейса (`tasks.go:848`), после чего tasks пересчитывает патч.

### 6.6 Связь с основным репозиторием

* Перед задачей основной репо должен быть чистым — тогда после Approve в ветку попадает ровно работа агента.
* `.edy/` прячется от основного git через `.git/info/exclude` (не через `.gitignore`, чтобы не менять файлы проекта).
* Restore теневого репо сбрасывает только рабочее дерево; основной `.git` (HEAD, индекс) не меняется.

---

## 7. MCP-путь — сводка

```
tasks.execute ──► Task.Run ──► attemptMCP
                                 │ Bind(root, MCPEvents) ──► api.mcpBridge: ключ → mcpBinding
                                 │ ctx += CallOptions{Workdir, MCP:url, MCPServers}
                                 ▼
                         OmniRoute.Chat (X-Edy-MCP, X-Edy-Workdir, X-Edy-MCP-Servers)
                                 ▼
                     omniroute CLI-адаптер: claude --mcp-config … --disallowedTools Bash Edit Write …
                                 ▼
                     Claude Code ──HTTP──► /mcp?project=…&bind=key ──► mcp.Server (bind)
                                              │ Workspace.Call → правка
                                              │ OpenShadow + Snapshot("MCP: …")
                                              └ OnCall / OnChange → события задачи
                                 ▼
                         ответ-итог ──► RunHooks ──► ok? / повтор с отчётом
```

После `release()` привязка удаляется, а MCP-сессии этого проекта без `agentID` (то есть сессии исполнителя)
закрываются (`mcpapi.go:240-249`).

---

## 8. Context Engine

### 8.1 Скелет (`codeindex.Build` + `Index.Skeleton`)

**Обход** (`codeindex.go:65-106`):

* Пропускаются каталоги из `skipDirs` (`:52`: `.git .edy node_modules vendor __pycache__ .venv venv dist build .idea .vscode`)
  **и любые скрытые** (имя начинается с `.`).
* Пропускаются файлы > `maxFileBytes = 512 KiB`, не-регулярные (симлинки и т.п.).
* Лимит `maxFiles = 3000` файлов (проверяется по `idx.Files`, куда Python-файлы попадают только после обхода —
  поэтому на Python-файлы лимит фактически не действует).
* `.go` → `parseGo`; `.py` → копятся и разбираются пачкой; текстовые расширения `textExt` (`:55`:
  `.md .txt .js .ts .tsx .jsx .html .css .json .yaml .yml .toml .sql .sh .mod .cfg .ini .proto`) и файлы
  `Makefile`, `Dockerfile` → `File{Lang:"text"}` без символов. **Всё остальное (в том числе `.swift`, `.rs`,
  `.java`, `.c`, `.kt`) не индексируется вообще.**
* Итог сортируется по пути.

**Go** (`parseGo`, `:125`): `parser.ParseFile` с `SkipObjectResolution`. Берутся только `FuncDecl`
(kind `func`/`method`) и `GenDecl` с `type` (kind `type`). Константы, переменные и `import` не попадают.
Сигнатура функции — `func (*Cart) Total() int` (тело не печатается); имя метода — `Cart.Total`.
`typeSummary` (`:164`): структура → `struct { Items, Owner }` (имена полей, встроенные — по типу),
интерфейс → `interface { Load, Save }`, прочее — текст типа, обрезанный до 80 байт.
Ошибка разбора → `Note: "не разобран: <первая строка ошибки>"`, символов нет.

**Python** (`parsePython`, `:209`): если есть `python3`, запускается `python3 -I -c <pyskeleton.py>` в корне
проекта (`-I` — изолированный режим, не подхватывает модули из проекта), пути — через stdin, таймаут 60 с на все файлы.
Скрипт (`pyskeleton.py`) обходит `def`/`async def`/`class` рекурсивно по телам классов; методы получают
имена `Class.method`; сигнатура — `def name(a, b, *args, **kw)` без аннотаций и значений по умолчанию;
классы — `class Name(Base1, Base2)` (через `ast.unparse`, если есть). Синтаксическая ошибка → `error`.
Если Python нет или скрипт упал/вернул мусор — для каждого файла `regexPython` (`:242`):
строки, подходящие под `^(\s*)(async\s+def|def|class)\s+(\w+)`, с `End = Line` (тела неизвестны).

**Формат скелета** (`Skeleton(limit)`, `:264`):

```
Файлов: 12

shop/cart.go (42 строк)
  5: type Cart struct { Items, Owner }
  10: func (*Cart) Total() int
  18: func ApplyDiscount(total, percent int) int

shop/broken.go (3 строк) — не разобран: shop/broken.go:3:1: expected …

users/repo.py (30 строк)
  4: class UserRepository(BaseRepo)
    5: def find_by_email(self, email, active)
README.md (2 строк)
```

Методы Python (имя с точкой) получают отступ 4 пробела, остальное — 2. Перед каждым файлом проверяется
`b.Len() > limit` (байты, не символы!); при превышении — «…(скелет обрезан, используйте search_code)».
Последний файл может вылезти за лимит на размер своих символов. Агенту даётся `limit = 30000`.

### 8.2 Нарезка на куски (`Index.Chunks`, `search.go:78-121`)

* Файл читается заново с диска (индекс содержит только метаданные).
* Для файлов с символами — один кусок на объявление верхнего уровня `[Line, End]`. Символ, начинающийся
  внутри уже покрытого диапазона (`Line <= covered`), пропускается — так методы Python покрываются своим классом.
  Код между объявлениями (импорты, константы, глобальные переменные) **в куски не попадает**.
* Для файлов без символов (тексты, неразобранные, пустые .py) — окна по `chunkLines = 60` строк.
* Термины куска = `tokenize(path + " " + symbol + " " + symbol + " " + text)` — путь и имя символа
  (дважды, то есть с весом) участвуют в поиске.

### 8.3 Токенизатор (`tokenize`, `search.go:39`)

* Слова — `[\p{L}\p{N}_]+` (Unicode, кириллица поддерживается).
* Слово < 2 рун отбрасывается; добавляется в нижнем регистре целиком.
* Если `splitIdent` даёт > 1 части — добавляются и части ≥ 2 рун: по `_` и по camelCase с учётом
  аббревиатур: `parseHTTPRequest` → `parsehttprequest, parse, http, request`; `user_name` → `user_name, user, name`.
* Стемминга нет (в отличие от памяти).

### 8.4 BM25 (`Index.Search`, `search.go:124-164`)

* Каждый запрос заново: `Chunks()` (чтение всех файлов) → df, средняя длина.
* `k1 = 1.2`, `b = 0.75`, `idf = ln(1 + (N − df + 0.5)/(df + 0.5))` (неотрицательный вариант).
* Повторяющиеся термины запроса учитываются многократно.
* Только `score > 0`, сортировка по убыванию, первые `k`.

### 8.5 Поиск по смыслу (`internal/semantic`)

**Включение.** Глобальная настройка `embed_model` (`SettingModel`, `semantic.go:33`) в формате
«провайдер/модель». Пусто — смысловой поиск выключен (везде остаётся BM25). `SetModel` (`:164`)
проверяет наличие `/` и сбрасывает кеш векторов. Устанавливается через `PUT /api/semantic`.

**Эмбеддинги.** `Client.Embed` (`:53`) — `POST <BaseURL>/embeddings` OmniRoute (формат OpenAI,
`{"model","input":[...]}`), заголовок `X-Edy-Client: embeddings` для учёта расходов, ответ ≤ 256 МБ.
Векторы раскладываются по полю `index` (порядок ответа не важен); если какой-то вектор пустой — ошибка
«модель вернула не все векторы». В проде клиент ходит в OmniRoute in-process (`internal/app/app.go:119`).

**Что эмбеддится.**
* Код: `codeText(chunk)` (`:218`) = `path · Symbol\n` + первые `embedChars = 1500` **байт** текста куска
  (с откатом до границы UTF-8 символа).
* Заметки памяти: текст заметки (`noteText`).

**Хранение.** Таблица SQLite `embeddings(project, key, hash, model, vec BLOB, updated)`, PK `(project, key)`
(`internal/store/embeddings.go:10-20`). Вектор — little-endian float32, **нормированный**.
Ключи: `code:<path>#<symbol>#<start>` и `note:<id>`. `hash` — SHA-1 hex от эмбеддимого текста.

**Инкрементальный индекс — `Engine.Index(ctx, project, root)`** (`:244-345`):

1. Нет модели → ошибка «модель эмбеддингов не выбрана».
2. Мьютекс на проект (параллельные индексации одного проекта сериализуются).
3. `codeindex.Build(root)` → куски → items; плюс все заметки проекта.
4. Старые векторы этой модели → карта `hash → vec`. Совпал хеш — вектор переиспользуется (даже если ключ
   другой, например функция переехала), иначе — в `todo`.
5. Статус: `Indexing=true, Total=len(items), Done=переиспользовано`.
6. `todo` батчами по `batchSize = 64` → `Embed` → `normalize` → в `save`; после каждого батча `Done += n`,
   `Embedded += n`. Первая ошибка останавливает цикл.
7. Если была ошибка — индекс неполный: в `keep` добавляются все старые ключи, а старые векторы без пары
   тоже сохраняются (ничего не теряется; при следующем запуске догоним).
8. `SaveEmbeddings(project, model, save, keep)` — upsert + удаление ключей не из `keep` и векторов
   другой модели.
9. Сброс кеша проекта, `Updated = now`, `Error` = ошибка, если была. Возвращается статус и ошибка.

**`Start(project, root, timeout)`** (`:348`) — запускает `Index` в горутине с собственным таймаутом
(`context.Background()`), если по статусу индексация не идёт. Вызывается из `POST /api/semantic/index`
с таймаутом 30 минут (`internal/api/semanticapi.go:81`).

**`Status(project)`** (`:179`) — копия статуса + текущая модель: `model, indexing, done, total, embedded, error, updated`.
Отдаётся `GET /api/semantic`.

**Поиск — `rank`** (`:421`): модель → векторы проекта по хешу (кеш в памяти `vectorsFor`, `:398`) →
эмбеддинг запроса (один HTTP-вызов на запрос) → косинус (= скалярное произведение нормированных векторов,
`dot`, при разной размерности −1) для текстов, **у которых есть вектор** с тем же хешем → top-k.
Тексты без вектора (новый/изменённый код, ещё не проиндексированный) в смысловой ранжировке не участвуют.

**RRF — `Fuse(k, lists...)`** (`:457`): score(key) = Σ 1/(60 + rank + 1), константа **60** (классическая
из статьи о RRF). Порядок ключей при равенстве — по первому появлению (stable sort).

**`semantic.Code.Search`** (`:487`, реализует `agent.CodeSearch`):

1. BM25: `idx.Search(query, 3k)`.
2. Нет движка или модели → BM25 (top k), `note = ""`.
3. Тексты всех кусков → `rank(…, 3k)` с таймаутом `Timeout` (по умолчанию 20 с).
4. Ошибка → BM25 + `note = "поиск по смыслу недоступен: <ошибка>"` (агент видит это пояснение).
5. Пустой результат (индекса ещё нет) → BM25.
6. Ключ куска `path#start`; списки BM25 и смысла объединяются `Fuse(k, …)`.

Индекс строится по `Project` (ключ в БД), а куски — из переданного `idx` (может быть worktree-копией):
благодаря хешам векторы основной папки подходят и для копии, если код не менялся.

**`semantic.Notes.Rank`** (`:545`, реализует `memory.Ranker`) — то же для заметок, таймаут 20 с;
любые ошибки → `nil` (молча остаётся BM25).

**Где подключено:**

| Место | Что |
|---|---|
| `internal/app/app.go:119` | `semantic.New(&semantic.Client{…InProcess(router)}, st)` → `h.Semantic`, передаётся в tasks и API. |
| `internal/tasks/tasks.go:482-491` `codeSearch` | Перед каждой задачей синхронно `Index(project)` с таймаутом 1 минута (ошибка игнорируется), затем `semantic.Code{E, Project}` в `Task.Code`. |
| `internal/tasks/tasks.go:465-479` `memoryFor` | `semantic.Notes` как `Ranker` для `memory.Access`. |
| `internal/api/mcpapi.go:155-158` | Внешние MCP-клиенты: `semantic.Code` и `semantic.Notes`. Исполнитель задачи через MCP получает `Code`/`Memory` из `MCPEvents`. |
| `internal/api/semanticapi.go` | `GET/PUT /api/semantic`, `POST /api/semantic/index` (`Start`), `GET /api/semantic/search` (гибрид, k=10, для проверки индекса в интерфейсе). |

### 8.6 Бюджет токенов — логика всего Context Engine

Основная идея — **не класть в промпт много**: модель получает каталог (скелет без тел — сотни строк вместо
десятков тысяч), а нужное достаёт сама:

* скелет ≤ 30000 байт;
* поиск возвращает ≤ 5 (max 20) кусков по ≤ 80 строк;
* `read_file` — до 200 КБ, но промпт советует читать файлы целиком «только когда это необходимо»;
* память — в промпт только закреплённое (≤ 1500 байт исполнителю, ≤ 2000 постоянному агенту), остальное — через `project_memory`;
* скиллы — в промпт только `имя — описание`, текст по `use_skill`;
* события и итоги обрезаются (600/200 символов), отчёт хуков — 4000 символов с конца.

---

## 9. Память проекта (`internal/memory`)

### 9.1 Модель данных

Заметка — `store.Note` (`internal/store/memory.go:31`): `ID, Project, Kind, Text, Source, Pinned, Agent, …`.
Виды (`store/memory.go:24-27`) и как они показываются (`kindTitle`, `memory.go:28`):

| Kind | Отображение | Кто пишет |
|---|---|---|
| `fact` | `[факт]` | агент через `remember`, пользователь |
| `lesson` | `[урок]` | Edy после провалов (см. 03) |
| `task` | `[задача]` | Edy — итог принятой задачи |
| `note` | `[заметка]` | пользователь |

`Agent` непуст — личная память постоянного агента; пусто — общая. Строка в выводе:
`- [вид] текст_в_одну_строку\n` (`line`, `memory.go:105`).

### 9.2 Поиск (`Search`, `memory.go:51`)

* Термины (`terms`, `:32`): нижний регистр, разбиение по не-буквам/не-цифрам, слова < 2 рун выбрасываются,
  **грубая основа**: слово ≥ 7 рун → первые 5, 4–6 рун → первые 4, 2–3 — как есть.
  Так «тесты»/«тестов» → `тест`, «возврат»/«возвраты» → `возвр`. Работает и для английского (`testing` → `testi`).
* BM25 с `k1=1.2, b=0.75` (в формуле `2.2 = k1+1`, `0.25 = 1−b`), idf как в codeindex.
* Возвращает до `n` заметок со `score > 0`.

### 9.3 Выжимки для промптов

| Функция | Что | Где используется |
|---|---|---|
| `Brief(notes, query, budget)` (`:111`) | Сначала закреплённые (по порядку, до первой не влезшей), затем до 8 похожих на `query`; без дублей; суммарно ≤ `budget` **байт**. | `api/projects.go:270` — память в чате Оркестратора (2000). |
| `Pinned(notes, budget)` (`:142`) | Только закреплённые **общие** (`Agent == ""`). | Исполнитель задачи: `tasks.go:478` (1500); `api/memoryapi.go:53` (превью). |
| `AgentBrief(notes, agentID, goal, budget)` (`:154`) | «Твоя память (опыт прошлых задач):» — личные заметки (закреплённые + похожие на задачу) в 2/3 бюджета; «Общее закреплённое:» — в остаток. | Постоянный агент: `tasks.go:476` (2000). |

Бюджет меряется `strings.Builder.Len()` — в байтах; кириллица — 2 байта на букву, поэтому «1500» — это
примерно 750 русских символов.

### 9.4 Доступ агента — `Access` (`memory.go:207`)

Реализует `agent.Memory`. Поля: `St`, `Project`, `Source` (например `task:<id>` или `mcp:<agentID>`),
`Agent` (личная память постоянного агента), `Limit` (0 → 5), `Ranker`.

* `Search(query)` (`:221`): заметки проекта (все, включая чужие личные) → BM25 top-8; если есть `Ranker` —
  `fuse(BM25, Ranker.Rank(…, 8), 8)` (RRF с той же константой 60, по `ID`).
* `Remember(text)` (`:244`): trim; пусто — ошибка; > `MaxNote = 400` рун — «запишите короче»;
  счётчик `written` под мьютексом против `Limit`; пишет `Kind=fact`, `Source`, `Agent`.
  Лимит считается на объект `Access` — tasks создаёт новый на каждую задачу (`tasks.go:471`), MCP — на сессию (`Limit: 20`).

Почему лимиты: память — не место для «простыней»; без ограничений слабые модели заваливают её
итогами задач (их Edy пишет сам — об этом сказано в описании `remember`).

---

## 10. Скиллы (`internal/skills`)

### 10.1 Формат

Совместим с Claude Code:

```
<папка скиллов>/
  go-tests/
    SKILL.md        ← основной формат
  style.md          ← допускается одиночный файл
```

`SKILL.md`:

```markdown
---
name: go-tests
description: Как писать тесты в этом проекте
---
Пиши табличные тесты…
```

Правила разбора (`parse`, `skills.go:117`; `splitFrontMatter`, `:140`):

* Файл > `maxBody = 64 KiB` — пропускается.
* BOM в начале снимается. Front matter — только если файл начинается с `---`; формат — простые строки
  `ключ: значение`, кавычки `"`/`'` по краям значения снимаются. Не закрыт `---` — весь файл считается текстом.
* `name` нет → имя папки (или имя файла без `.md`).
* Имя должно соответствовать `^[\p{L}\p{N}][\p{L}\p{N}_.-]{0,63}$` (`ValidName`) — буквы любых алфавитов,
  цифры, `_ . -`, до 64 символов, не может начинаться с точки (защита от `../`). Неверное — скилл пропускается.
* `description` нет → первая строка тела без `#` и пробелов, до 120 рун.

### 10.2 Папки и приоритет

`Dirs(home, project)` (`:46`) — по возрастанию приоритета:
1. `<home>/skills` — общие (`Scope = "global"`);
2. `<project>/.edy/skills` (`ProjectDir`) — проекта (`Scope = "project"`).

`Load(dirs)` (`:72`): несуществующие папки пропускаются; битые скиллы пропускаются (один плохой не ломает
остальные); при совпадении имён побеждает более поздняя папка (проект перекрывает общий); результат
отсортирован по имени.

### 10.3 Остальное API

* `Find(list, name)` (`:108`) — точное совпадение имени.
* `Save(dir, s)` (`:181`) — валидирует имя, непустое тело ≤ 64 КБ; пишет `<dir>/<name>/SKILL.md` с front matter
  (переводы строк в description заменяются пробелами). Возвращает путь.
* `Remove(s)` (`:201`) — если `Path` оканчивается на `SKILL.md`, удаляется вся папка скилла (`RemoveAll`), иначе файл.
* `Catalog(list)` (`:209`) — текст для системного промпта:
  ```
  Доступные скиллы (инструкции этого проекта). Если скилл подходит к задаче, сначала загрузи его через use_skill:
  - go-tests — Тесты этого проекта
  ```

Какие скиллы попадут исполнителю, решает tasks (`tasks.go:437-449`): все (`AllSkills`) или выданные
Оркестратором по именам. Через MCP — `list_skills`/`use_skill` и MCP-prompts (см. 04-mcp.md).

---

## 11. Экспортируемый API по файлам

### 11.1 `internal/agent/task.go`

| Символ | Строка | Описание |
|---|---|---|
| `StatusDone`, `StatusNeedsHuman`, `StatusFailed` | `task.go:21-23` | Статусы `Result`. |
| `Event` | `task.go:27` | Запись ленты: время, вид, текст. |
| `Task` | `task.go:34` | Задача агенту (см. §2.2). |
| `MCPBridge` | `task.go:72` | `Supports(model)`, `Bind(root, ev) (url, release)`. |
| `MCPEvents` | `task.go:82` | Скиллы, память, поиск и колбэки для MCP-сервера, привязанного к задаче. |
| `Result` | `task.go:93` | Итог задачи (см. §2.7). |
| `(*Task).Run` | `task.go:121` | Выполнить задачу целиком с цепочкой моделей. |
| `CheckClean` | `task.go:455` | Проверить, что проект готов к задаче (git, чистота). |
| `Branch` | `task.go:483` | Текущая ветка основного репо. |
| `BranchName` | `task.go:510` | Имя ветки `edy/<slug>-MMDD-HHMM`. |
| `HasChanges` | `task.go:523` | Есть ли незакоммиченные изменения. |
| `FreeBranch` | `task.go:533` | Свободное имя ветки (`-2`, `-3`, …). |
| `Approve` | `task.go:545` | Новая ветка + один коммит со всеми изменениями. |
| `Reject` | `task.go:560` | Откат проекта к снимку baseline. |

Неэкспортируемые, но важные: `emit` (`:102`), `systemPrompt` (`:108`), `attempt` (`:224`), `imagesFor` (`:301`),
`memoryNote` (`:314`), `mcpPrompt` (`:329`), `attemptMCP` (`:343`), `argString` (`:418`), `firstLine` (`:427`),
`gitIn` (`:437`), `isRepo` (`:448`), `excludeEdyDir` (`:488`), `translit` (`:570`).

### 11.2 `internal/agent/workspace.go`

| Символ | Строка | Описание |
|---|---|---|
| `Workspace` | `workspace.go:32` | Папка проекта + скиллы, память, поиск кода. |
| `CodeSearch` | `workspace.go:44` | `Search(idx, query, k) (hits, note)` — подключаемый поиск кода. |
| `Memory` | `workspace.go:49` | `Search(query) string`, `Remember(text) error`. |
| `(Workspace).Tools` | `workspace.go:94` | JSON-описания инструментов для модели. |
| `(Workspace).Call` | `workspace.go:126` | Выполнить инструмент; `changed` — нужен снимок. |
| `(Workspace).Files` | `workspace.go:291` | Файлы проекта без служебных папок, не больше `limit` (для интерфейса). |
| `(Workspace).ReadText` | `workspace.go:320` | Прочитать текстовый файл для просмотра: ≤ `limit` байт, без двоичных, обрезка по границе UTF-8. |

Неэкспортируемые: `maxReadBytes/maxListed/skeletonLimit` (`:21-25`), `skipDirs` (`:28`), `resolve` (`:55`),
`list` (`:243`), `unwrapPath` (`:282`).

### 11.3 `internal/agent/hooks.go`

| Символ | Строка | Описание |
|---|---|---|
| `Hook` | `hooks.go:19` | `{name, cmd[]}` — проверка без ИИ. |
| `HooksFile` | `hooks.go:25` | `.edy/hooks.json`. |
| `DetectHooks` | `hooks.go:29` | Хуки из файла или по стеку. |
| `RunHooks` | `hooks.go:85` | Запустить по очереди, вернуть ok и отчёт. |

Неэкспортируемые: `hookTimeout` (`:16`), `exists` (`:55`), `hasPython` (`:60`), `hasPythonTests` (`:65`), `lastRunes` (`:109`).

### 11.4 `internal/agent/shadow.go`

| Символ | Строка | Описание |
|---|---|---|
| `Shadow` | `shadow.go:18` | Теневой репозиторий проекта `Root`. |
| `Checkpoint` | `shadow.go:23` | Снимок: id, сообщение, время. |
| `(Shadow).Init` | `shadow.go:53` | Создать/починить `.edy/shadow.git`. |
| `(Shadow).Snapshot` | `shadow.go:73` | Снимок текущего состояния, вернуть id. |
| `(Shadow).Restore` | `shadow.go:85` | Вернуть весь проект к снимку. |
| `(Shadow).List` | `shadow.go:97` | Список снимков, новые первыми. |
| `(Shadow).Diff` | `shadow.go:115` | `--stat` от снимка. |
| `OpenShadow` | `shadow.go:124` | Подготовить Shadow для правок вне задачи (MCP). |
| `(Shadow).Patch` | `shadow.go:135` | Полный diff от снимка, обрезанный до `limit`. |
| `(Shadow).RestoreFile` | `shadow.go:151` | Вернуть один файл к снимку (или удалить созданный). |

Неэкспортируемые: `gitDir` (`:29`), `git` (`:31`).

### 11.5 `internal/agent/llm.go`

| Символ | Строка | Описание |
|---|---|---|
| `Msg` | `llm.go:18` | Сообщение OpenAI chat; `Images` не сериализуются напрямую. |
| `(Msg).MarshalJSON` | `llm.go:28` | Сообщение с картинками — массив частей text/image_url. |
| `ToolCall` | `llm.go:43` | Вызов инструмента от модели (`id`, `function.name/arguments`). |
| `ToolDef` | `llm.go:53` | Описание инструмента (`type: function`, JSON Schema параметров). |
| `CallOptions` | `llm.go:64` | `Workdir`, `MCP`, `MCPServers` для CLI-провайдеров. |
| `WithCallOptions` | `llm.go:74` | Положить `CallOptions` в контекст. |
| `LLM` | `llm.go:84` | `Chat(ctx, model, msgs, tools) (Msg, error)`. |
| `OmniRoute` | `llm.go:90` | Клиент `/v1/chat/completions`: `BaseURL`, `Token`, `Client` (X-Edy-Client), `HTTP`. |
| `(*OmniRoute).Chat` | `llm.go:97` | Запрос без стриминга; ответ ≤ 32 МБ; ошибка «модель X: <message>». |
| `InProcess` | `llm.go:170` | `http.Client`, вызывающий `http.Handler` напрямую (через `httptest.Recorder`). |

### 11.6 `internal/codeindex`

| Символ | Строка | Описание |
|---|---|---|
| `Symbol` | `codeindex.go:29` | Объявление: kind (`func/method/type/def/class`), имя, сигнатура, строки. |
| `File` | `codeindex.go:38` | Файл: путь, язык (`go/python/text`), число строк, символы, `Note`. |
| `Index` | `codeindex.go:47` | Корень и файлы. |
| `Build` | `codeindex.go:65` | Разобрать проект. |
| `(*Index).Skeleton` | `codeindex.go:264` | Текст скелета ≤ `limit` байт (примерно). |
| `Chunk` | `search.go:15` | Кусок для поиска (+ скрытые `terms`, `length`). |
| `Hit` | `search.go:26` | Найденный кусок со `Score`. |
| `(*Index).Chunks` | `search.go:78` | Нарезка проекта на куски. |
| `(*Index).Search` | `search.go:124` | BM25 top-k. |
| `FormatHits` | `search.go:167` | Текст результатов для модели. |

Неэкспортируемые: `skipDirs` (`codeindex.go:52`), `textExt` (`:55`), `maxFiles/maxFileBytes/chunkLines` (`:58-62`),
`countLines` (`:108`), `parseGo` (`:125`), `typeSummary` (`:164`), `nodeString` (`:194`), `pySkeleton` (`:203`),
`pyDefRe` (`:205`), `parsePython` (`:209`), `regexPython` (`:242`), `tokenize` (`search.go:39`), `splitIdent` (`search.go:59`).

### 11.7 `internal/semantic/semantic.go`

| Символ | Строка | Описание |
|---|---|---|
| `SettingModel` | `:33` | Ключ настройки `embed_model`. |
| `Embedder` | `:41` | `Embed(ctx, model, texts) ([][]float32, error)`. |
| `Client` | `:46` | Эмбеддинги через OmniRoute. |
| `(*Client).Embed` | `:53` | Запрос `/embeddings`, раскладка по `index`. |
| `Store` | `:114` | Векторы, заметки, настройки (реализует `*store.Store`). |
| `Status` | `:123` | Состояние индекса проекта. |
| `Engine` | `:134` | Индекс и поиск; статусы, блокировки, кеш. |
| `New` | `:149` | Создать `Engine`. |
| `(*Engine).Model` | `:154` | Текущая модель эмбеддингов (nil-безопасно). |
| `(*Engine).SetModel` | `:164` | Сменить модель, сбросить кеш. |
| `(*Engine).Status` | `:179` | Статус индекса проекта. |
| `(*Engine).Index` | `:244` | Построить/обновить индекс (синхронно). |
| `(*Engine).Start` | `:348` | Индексация в фоне. |
| `Fuse` | `:457` | Reciprocal rank fusion списков ключей (константа 60). |
| `Code` | `:477` | Гибридный поиск кода (`agent.CodeSearch`). |
| `(Code).Search` | `:487` | BM25 + смысл → RRF; при сбое — BM25 и пояснение. |
| `Notes` | `:540` | Ранжирование заметок по смыслу (`memory.Ranker`). |
| `(Notes).Rank` | `:545` | Top-k заметок по косинусу; ошибки → `nil`. |

Неэкспортируемые: `embedChars`/`batchSize` (`:36-37`), `hash` (`:212`), `codeText` (`:218`), `utf8Start` (`:234`),
`noteText` (`:236`), `normalize` (`:370`), `dot` (`:386`), `vectorsFor` (`:398`), `rank` (`:421`), `chunkKey` (`:483`), `trim` (`:532`).

### 11.8 `internal/memory/memory.go`

| Символ | Строка | Описание |
|---|---|---|
| `MaxNote` | `:20` | 400 — максимальная длина заметки в рунах. |
| `Store` | `:23` | `AddNote`, `Notes`. |
| `Search` | `:51` | BM25 по заметкам, до n. |
| `Brief` | `:111` | Закреплённое + похожее, ≤ budget байт. |
| `Pinned` | `:142` | Только закреплённые общие. |
| `AgentBrief` | `:154` | Личная память постоянного агента + общее закреплённое. |
| `Ranker` | `:175` | `Rank(query, notes, k)` — поиск по смыслу. |
| `Access` | `:207` | Доступ агента: поиск и запись с лимитом. |
| `(*Access).Search` | `:221` | Текст результатов `project_memory`. |
| `(*Access).Remember` | `:244` | Записать факт. |

Неэкспортируемые: `kindTitle` (`:28`), `terms` (`:32`), `line` (`:105`), `fuse` (`:180`).

### 11.9 `internal/skills/skills.go`

| Символ | Строка | Описание |
|---|---|---|
| `Skill` | `:28` | Имя, описание, тело, путь, область. |
| `Global`, `Project` | `:38-39` | Значения `Scope`. |
| `ProjectDir` | `:43` | `.edy/skills`. |
| `Dirs` | `:46` | Папки скиллов по возрастанию приоритета. |
| `Dir` | `:58` | Путь + область. |
| `ValidName` | `:68` | Проверка имени. |
| `Load` | `:72` | Загрузить и слить скиллы. |
| `Find` | `:108` | Найти по имени. |
| `Save` | `:181` | Сохранить как `<имя>/SKILL.md`. |
| `Remove` | `:201` | Удалить скилл. |
| `Catalog` | `:209` | Список для системного промпта. |

Неэкспортируемые: `maxBody` (`:63`), `validName` (`:65`), `parse` (`:117`), `splitFrontMatter` (`:140`), `firstLine` (`:171`).

---

## 12. Тесты

Запуск:

```bash
go test ./internal/agent/ ./internal/codeindex/ ./internal/semantic/ ./internal/memory/ ./internal/skills/
go test -run TestCircuitBreaker -v ./internal/agent/   # один тест
```

Требования: `git` в PATH (иначе тесты агента пропускаются через `t.Skip`); `python3` желателен для
`codeindex` (без него часть проверок Python идёт через запасной разбор); `go` нужен тестам агента,
потому что хуки реально запускают `go vet`/`go test` во временном проекте. Тесты агента идут ~10–15 с.
Тесты задают `GIT_AUTHOR_*`, `GIT_COMMITTER_*` и `GIT_CONFIG_GLOBAL=/dev/null`, чтобы не зависеть от
настроек машины (`agent_test.go:28-34`).

Фикстура агента: `newProject` — временный git-репозиторий с `go.mod`, `calc.go` (ошибка: `a - b`) и
`calc_test.go`. Фальшивая модель `scripted` отдаёт заранее заданные ответы по имени модели и запоминает,
какие сообщения видела (`seen`). `fixSteps` — правильное решение, `failSteps` — мусорный файл и три `finish`.

| Тест | Файл:строка | Что проверяет |
|---|---|---|
| `TestTaskFixesBugAndApprovesIntoBranch` | `agent_test.go:118` | Полный успешный цикл: статус/модель/diff, модель получила содержимое файла, 3 снимка (Начало, Шаг, Готово), `BranchName` с транслитом, `Approve` создаёт ровно 1 коммит только с `calc.go`, `main` не меняется, `.edy` не в git. |
| `TestCircuitBreakerEscalatesAndRollsBack` | `:168` | Слабая модель 3 раза проваливает тесты → мусор откачен → сильная справляется; события breaker; сильная начинает с чистой истории; слабая получала вывод упавшего теста. |
| `TestNeedsHumanRestoresProject` | `:206` | Обе модели провалились → `needs_human`, код и рабочая копия как до задачи. |
| `TestRejectRestoresBaseline` | `:227` | `Reject` после успешной задачи возвращает исходный код. |
| `TestDirtyRepoIsRefused` | `:241` | Незакоммиченные изменения → ошибка «незакоммиченные». |
| `TestWorkspaceConfinement` | `:250` | Запрет `../x`, `/etc/passwd`, `.git/config`, `.edy/...`, симлинка наружу, `a/../../x` для чтения и записи; запись внутрь работает; `list_files` не показывает `.git`; пустой `old` отклоняется. |
| `TestDetectHooks` | `:277` | Пустой проект → нет хуков; `go.mod` → go vet + go test; `.edy/hooks.json` перекрывает. |
| `TestOmniRouteClient` | `:294` | Заголовки `Authorization`/`X-Edy-Client`, 8 инструментов, разбор tool_calls при `content: null`, текст ошибки из `error.message`. |
| `TestPlainFolderWithoutGit` | `:319` | Папка без git: задача работает, правка остаётся, `OnAttempt/OnAttemptEnd` вызываются, `Reject` работает через Shadow Git. |
| `TestContextTools` | `:346` | `project_skeleton` без тел, `search_code` находит `Add`, пустой запрос отклоняется. |
| `TestWorkspaceSkills` | `:363` | `use_skill` появляется только при наличии скиллов, возвращает тело, ошибка для неизвестного. |
| `TestAgentOnTextOnlyCLIThroughOmniRoute` | `integration_test.go:29` | Сквозной: реальный `omniroute.NewServer` + shell-скрипт «CLI» без tool calling (эмуляция `<tool_call>`), задача доходит до `✓ go test`. |
| `TestSkeleton` | `codeindex_test.go:73` | Формат скелета Go/Python, структуры/интерфейсы, `async def`, неразобранный файл, игнор `node_modules` и скрытых папок, нет тел, обрезка по лимиту. |
| `TestSearch` | `codeindex_test.go:107` | BM25 находит нужный символ первым (в т.ч. смешанный рус/англ запрос), формат `FormatHits`, пустой результат. |
| `TestTokenize` | `codeindex_test.go:130` | Разбиение camelCase с аббревиатурой, snake_case, кириллица. |
| `TestPythonWithoutPython3` | `codeindex_test.go:139` | `PATH` без python3 → запасной разбор регулярным выражением. |
| `TestIndexIncrementalAndSearch` | `semantic_test.go:81` | Без модели индекс не строится; модель без `/` отклоняется; первый индекс считает всё, повторный — ничего, изменение одного куска — один пересчёт, удалённые ключи чистятся; гибрид находит `checkCredentials` по «пароль»; заметки по смыслу; ошибка эмбеддингов → BM25 + пояснение, старые векторы сохраняются, ошибка в статусе; выключенная модель → только слова без пояснений. Эмбеддер `fakeEmb` кодирует «понятия» в оси вектора. |
| `TestFuse` | `semantic_test.go:159` | Порядок RRF. |
| `TestClient` | `semantic_test.go:166` | Раскладка ответа по `index` (обратный порядок), ошибка API, неполный ответ отклоняется. |
| `TestSearchAndBrief` | `memory_test.go:19` | BM25 с основами слов, `Brief` (закреплённое + похожее, бюджет), `Pinned`. |
| `TestAccess` | `memory_test.go:45` | Пустая память, длинная заметка, лимит записей, поиск по записанному, `Source`/`Kind`. |
| `TestAccessRanker` | `memory_test.go:74` | Слияние BM25 и `Ranker`. |
| `TestLoadMergesGlobalAndProject` | `skills_test.go:20` | Перекрытие общего скилла проектным, одиночный `.md`, описание из первой строки, отбраковка `../evil`, не-`.md` файлы, `Catalog`. |
| `TestSaveAndRemove` | `skills_test.go:48` | Валидация имени и тела, кириллическое имя, перенос строки в описании, удаление. |

Чего тестами **не** покрыто: `attemptMCP` (покрыт косвенно в `internal/tasks`/`internal/api`, см. 03/04),
`MaxSteps`, отмена посреди задачи, `RestoreFile`, `Patch`, `FreeBranch`, `ReadText`, картинки/`CanSee`,
`Engine.Start`, `AgentBrief`, CRLF в front matter.

---

## 13. Известные ограничения и «грабли»

### 13.1 Найденные баги / странности

1. **`Engine.Start` может навсегда «залипнуть» в `Indexing=true`** (`semantic.go:348-368`). `Start` сам ставит
   `Indexing = true`, а сбрасывает его только `defer` внутри `Index`, который регистрируется *после* проверки
   модели, `codeindex.Build` и `St.Embeddings` (`:245-290`). Если любая из них вернёт ошибку (модель
   сбросили между проверкой в API и запуском, папка проекта исчезла, ошибка БД) — `Indexing` останется `true`,
   и все последующие `Start` для проекта будут молча игнорироваться до перезапуска Edy. Исправление:
   в горутине `Start` после `Index` всегда делать `setStatus(…Indexing=false)`.
2. **Регистр `.git`/`.edy` в `resolve`** (`workspace.go:66-69`): на регистронезависимой ФС macOS путь `.GIT/config`
   или `.EDY/hooks.json` обходит запрет. Нужно сравнивать через `strings.EqualFold`.
3. **Висячий симлинк наружу** (`workspace.go:76-89`): если симлинк внутри проекта указывает на
   несуществующий файл вне проекта, `write_file` создаст файл снаружи. Нужно для существующего пути
   проверять `Lstat` и запрещать запись через симлинк, если `EvalSymlinks` падает.
4. **`MaxSteps` — на модель, а не на попытку** (`task.go:231-240`): комментарий у поля говорит «на одну попытку»,
   но счётчик `steps` не сбрасывается после провала хуков. При превышении `OnAttemptEnd` получит заметку
   «не прошла проверки», хотя проверки могли и не запускаться.
5. **Ошибки модели без паузы** (`task.go:242-249`, `:389-398`): при сетевой ошибке/429 цикл сразу повторяет запрос;
   3 мгновенные ошибки — и модель «провалена», проект откатывается. Ротация ключей и повторы есть в OmniRoute,
   но на уровне агента backoff не помешал бы.
6. **`attempt` не проверяет отмену после ошибки `Chat`** (в отличие от `attemptMCP`): если отмена совпала
   с третьей ошибкой, модель засчитывается как провалившая и делается откат; следующая итерация всё равно вернёт
   `ctx.Err()`, так что итог корректен, но в ленте будет лишний «провал».
7. **`codeindex` не учитывает Python-файлы в `maxFiles`** (`codeindex.go:78`) — в большом Python-проекте лимит 3000
   не работает; кроме того, все `.py` уходят одним вызовом python3 с общим таймаутом 60 с — при таймауте весь
   проект откатывается на regex-разбор.
8. **Нет поддержки Swift/Rust/Java/C/Kotlin** в `codeindex` (`textExt`, `codeindex.go:55`): такие файлы не попадают
   ни в скелет, ни в поиск, ни в смысловой индекс. Для IDE под macOS это заметно — добавление `.swift` хотя бы как
   `text` (куски по 60 строк) — дешёвый первый шаг.
9. **Бюджеты в байтах, а не символах**: `Skeleton(limit)`, `Brief/Pinned/AgentBrief(budget)`, `embedChars` — всё
   через `len()`/`Builder.Len()`. Для русского текста фактический объём вдвое меньше ожидаемого.
10. **`read_file` режет по байтам** (`workspace.go:193-194`) — может разрезать UTF-8 символ и не отсекает двоичные файлы
    (в отличие от `ReadText`).
11. **`replace_in_file`/`write_file` пишут с правами `0644`** — исполняемые файлы теряют `+x`.
12. **Shadow не откатывает игнорируемые файлы** (`shadow.go:92`, `clean -fd` без `-x`): если агент создал/изменил
    файл из `.gitignore` (например `.env`, `dist/…`), Reject/Circuit Breaker его не вернут. Аналогично, если агент
    допишет что-то в `.gitignore` проекта, новые файлы, подпавшие под правило, не будут удалены откатом.
13. **`FreeBranch` после 99 попыток** возвращает `name-99`, даже если она занята (`task.go:535-541`) — Approve тогда упадёт.
14. **`splitFrontMatter` и CRLF** (`skills.go:140-170`): `bufio.Scanner` отрезает `\r`, а смещение `consumed` считается
    по укороченным строкам — у файла с Windows-переводами строк начало тела «съедет» на несколько байт
    (в тело попадёт хвост заголовка). На практике безвредно, но заметно.
15. **Повторное событие о картинках в `attemptMCP`** — `imagesFor` вызывается на каждой итерации.
16. **`Result.Diff` — только `--stat`**; полный патч надо брать через `Shadow.Patch`.
17. **Индекс кода строится заново на каждый `project_skeleton`/`search_code`**, а `Search` ещё и перечитывает все файлы
    (`Chunks`). На больших проектах каждый вызов — секунды. Кеш по mtime — очевидное улучшение.
18. **Смысловой поиск не видит изменённый агентом код**: векторы ищутся по хешу текста, а индекс обновляется только
    перед задачей (`tasks.codeSearch`). Новые/изменённые куски находятся только через BM25-часть гибрида.
19. **Расхождения с `docs/ARCHITECTURE.md`**: там Shadow Git — go-git в `.edy/shadow`, векторы — chromem-go,
    «блокировка файлов». В коде — git CLI, `.edy/shadow.git`, SQLite-таблица `embeddings` и запрет MCP-правок на время задачи.

### 13.2 Грабли при доработке

* **Не используйте контекст задачи для git снимков без `WithoutCancel`.** Любая новая git-команда в `Shadow` должна идти
  через `Shadow.git`. Убитый посередине `git add` оставит `index.lock`, и откат перестанет работать.
* **Не пишите в `.gitignore` проекта** — для скрытия `.edy` используется `.git/info/exclude`. Задача требует чистого
  репозитория: любое собственное изменение Edy в рабочей копии сломает `CheckClean` следующей задачи.
* **Новый инструмент агента** добавляется в трёх местах: описание в `Tools()`, обработка в `Call()` (верните
  `changed=true`, если меняет файлы — иначе не будет снимка) и, если он не должен попадать в MCP или должен быть
  read-only, — `readOnly`/`noFiles` в `internal/mcp/mcp.go:209-213`. Не забудьте тест `TestOmniRouteClient`
  (считает инструменты) и эмуляцию tool calling в OmniRoute (`toolemu.go`), которая передаёт описания текстом.
* **Пути в инструментах — только через `resolve`.** `RestoreFile` дополнительно склеивает `Root` и `file` напрямую —
  это безопасно только потому, что перед этим вызван `resolve`.
* **`Task` мутирует сам себя** (`MaxAttempts`, `MaxSteps` по умолчанию) — не переиспользуйте один `Task` для двух `Run`
  одновременно.
* **Порядок моделей в `Models`** — это и есть Circuit Breaker. Каждая следующая начинает с нуля; знания о провале
  предыдущей она **не получает** (сознательно: «чистый лист», но это можно улучшить — см. TODO).
* **Промпты на русском** и тесты местами проверяют подстроки событий (`"провалила проверки 3 раза подряд"`,
  `"передана модели"`, `"Проверки не прошли"`, `"незакоммиченные"`). Меняя тексты — обновляйте тесты.
* **`DetectHooks` запускается один раз в начале `Run`**: если агент создаст `go.mod` или тесты Python, новые хуки
  в этой задаче не появятся.
* **Хуки исполняют код проекта на машине пользователя без песочницы** (решение из ARCHITECTURE.md). Агент может написать
  тест, который сделает что угодно — это осознанный риск личного инструмента.
* **Память общая для всех задач проекта**, а `Access.Search` ищет и по чужим личным заметкам постоянных агентов
  (фильтр по `Agent` есть только в выжимках).
* **Смена модели эмбеддингов** делает старые векторы бесполезными: они удалятся при следующей индексации
  (`SaveEmbeddings` удаляет векторы другой модели), а до неё смысловой поиск пуст и молча падает в BM25.

### 13.3 Идеи (TODO)

* Исправить баги 1–3 из §13.1 (залипание `Start`, регистр, висячие симлинки) — небольшие и важные.
* Передавать следующей модели короткий «урок» от провалившейся (что пробовали, какие тесты падали) —
  сейчас это пишется только в память после задачи.
* Кешировать `codeindex.Index` по mtime файлов и инкрементально переиндексировать изменённые агентом файлы
  (в том числе векторы) прямо во время задачи.
* Добавить `.swift`, `.rs`, `.java`, `.kt`, `.c/.h`, `.cpp` хотя бы как текст; для Swift — разбор через
  `swift-syntax`/`sourcekitten`, если установлены (по аналогии с python3).
* Включать в куски «шапку» файла (imports/const/var) для Go и Python.
* `read_file` с диапазоном строк (`start`, `end`) — заметно экономит токены по сравнению с чтением целиком.
* Экспоненциальная пауза между ошибками `Chat` и отдельный счётчик сетевых ошибок, не «съедающий» модель.
* Бюджеты в рунах вместо байт.
* Сохранять права файла в `replace_in_file`.
* `clean -fdx` с исключениями (или явный список созданных агентом файлов) для полного отката.
* Тесты на `attemptMCP`, `MaxSteps`, отмену, `RestoreFile`, `Patch`, `Engine.Start`.
