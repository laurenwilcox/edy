# 04. MCP: сервер Edy, HTTP-транспорт, каталог внешних серверов, Claude Code

Состояние описано на коммит `b88fcbf`. Все ссылки вида `файл:строка` — на этот коммит.
Если код уже сдвинулся, ищите по имени функции: имена в этом документе точные.

Документ отвечает на вопросы:

- как устроен MCP-сервер Edy и что именно он отдаёт клиентам (раздел 2–4);
- как работают HTTP-транспорт `/mcp`, сессии и привязки к задачам (раздел 5–6);
- как исполнитель-Claude Code получает MCP Edy и внешние серверы (раздел 7);
- как хранится и выдаётся каталог внешних MCP-серверов с секретами (раздел 8);
- как подключить к Edy внешний клиент — Claude Code, Cursor, Gemini CLI (раздел 9);
- модель безопасности, тесты, известные ошибки и идеи (раздел 10–14).

---

## 1. Зачем это всё и общая картина

MCP (Model Context Protocol) — JSON-RPC 2.0 протокол, по которому ИИ-клиент (Claude Code, Cursor,
Gemini CLI, Claude Desktop…) получает «инструменты» от внешнего сервера. В Edy MCP используется
**в двух направлениях**:

1. **Edy — MCP-сервер.** Внешние агенты работают с проектом инструментами Edy: скелет проекта,
   поиск по коду, чтение и правка файлов только внутри проекта, проверки (хуки), скиллы, память.
   Каждая правка сохраняется снимком в Shadow Git (`.edy/shadow.git`), и её можно откатить из
   интерфейса Edy. Это используется:
   - пользователем: он подключает Edy к своему Claude Code / Cursor вручную (раздел 9);
   - самим Edy: когда задачу исполняет Claude Code, Edy запускает `claude` с `--mcp-config`,
     указывающим на свой же `/mcp`, и запрещает Claude Code его собственные инструменты правки
     (раздел 7). Так все правки исполнителя идут через Edy → снимки + журнал.
2. **Edy — хранитель каталога внешних MCP-серверов** (Playwright, Context7, GitHub, приватные
   серверы с токенами). Оркестратор по описанию решает, какие из них выдать исполнителю под
   задачу, и Edy подкладывает их в тот же `--mcp-config` Claude Code (раздел 8).

```
                     ┌──────────────────────── процесс Edy (edy serve / Edy.app) ───────────────────────┐
 Claude Code (польз.)│                                                                                   │
  --transport http ──┼─► /mcp?project=P ──► api.serveMCP ──► mcpSession ──► mcp.Server ──► agent.Workspace│
                     │        (mcpapi.go)        │  токен, knownProject       (mcp.go)       (файлы, поиск)│
 edy mcp (stdio) ────┼─► bridge.forward ─────────┘                                │                      │
  (cmd/edy/mcp.go)   │                                                            └─► Shadow Git снимок   │
                     │                                                                                   │
                     │  tasks → agent.Task.attemptMCP ──Bind()──► URL /mcp?project=R&bind=KEY            │
                     │        │                                                                          │
                     │        └─► OmniRoute (in-process) с заголовками X-Edy-MCP, X-Edy-MCP-Servers      │
                     │               └─► cli.go: claude --mcp-config tmp.json --strict-mcp-config …      │
                     └──────────────────────────────────────────┬────────────────────────────────────────┘
                                                                │ (подпроцесс в PTY)
                                                     claude ────┘──► POST /mcp?project=R&bind=KEY  (обратно в Edy)
```

---

## 2. Файлы и их назначение

| Файл | Строк | Назначение |
|---|---|---|
| `internal/mcp/mcp.go` | 349 | Сам MCP-сервер: разбор JSON-RPC, `initialize`, `tools/list`, `tools/call`, `prompts/*`, блокировка правок (`Busy`), снимки через Shadow, транспорт stdio. Транспорт-независим: на вход байты одного сообщения, на выход байты ответа. |
| `internal/mcp/mcp_test.go` | 205 | Юнит-тесты сервера (протокол, снимки, выход за проект, `Busy`, скиллы, stdio, память). |
| `internal/api/mcpapi.go` | 374 | HTTP-транспорт `/mcp` (Streamable HTTP без SSE), сессии `Mcp-Session-Id`, вытеснение старых сессий, привязки задач (`bind=`), `MCPBridge` для исполнителей, журнал «Агенты» для внешних клиентов. Во второй половине файла (с `:270`) — HTTP API скиллов `/api/skills` (к MCP относится косвенно). |
| `internal/api/mcpapi_test.go` | — | Тест HTTP-транспорта и журнала (`TestMCPEndpointJournalsSessions`). |
| `internal/api/mcpcatalog.go` | 178 | HTTP API каталога внешних MCP-серверов `/api/mcp-servers` + пресеты (Playwright, Context7, Fetch, GitHub). Секреты наружу не отдаёт. |
| `internal/api/mcpcatalog_test.go` | — | Тесты каталога и выдачи серверов Оркестратором. |
| `internal/store/mcpservers.go` | 139 | Таблица `mcp_servers`, тип `MCPServer`, валидация, шифрование конфигурации (`vault.Seal`). |
| `internal/omniroute/climcp.go` | 135 | Как CLI-утилите (сейчас только `claude`) передать MCP: временный `--mcp-config`, `--strict-mcp-config`, `--allowedTools`, `--disallowedTools`; проверка адреса MCP (только localhost); разбор заголовка `X-Edy-MCP-Servers`. |
| `internal/omniroute/climcp_test.go` | — | `TestMCPAttachRules`. |
| `cmd/edy/mcp.go` | 165 | Команда `edy mcp -project DIR`: MCP по stdio. Если Edy уже запущен — мост в его `/mcp` (журнал, блокировки); иначе — автономный сервер. |
| `cmd/edy/mcp_test.go` | 66 | Тесты моста и автономного режима. |
| `internal/app/mcp_e2e_test.go` | 199 | Сквозной тест: поддельный `claude` (тот же тестовый бинарник), задача через API, правка через MCP Edy, снимок, внешний сервер из каталога. |

Смежный код, без которого картина неполная:

| Где | Что |
|---|---|
| `internal/agent/workspace.go:94-124` | `Workspace.Tools()` — описания и JSON-схемы большинства инструментов MCP (MCP-сервер берёт их отсюда). |
| `internal/agent/workspace.go:126-245` | `Workspace.Call()` — реализация инструментов. |
| `internal/agent/workspace.go:54-91` | `Workspace.resolve()` — запрет выхода за проект, `.git`, `.edy`, симлинки наружу. |
| `internal/agent/task.go:71-90` | Интерфейсы `MCPBridge`, `MCPEvents`. |
| `internal/agent/task.go:182-187` | Выбор: `attemptMCP` (утилита с MCP) или обычный цикл с эмуляцией инструментов. |
| `internal/agent/task.go:331-411` | `mcpPrompt` и `attemptMCP` — работа исполнителя через MCP. |
| `internal/agent/llm.go:63-69, 116-124` | `CallOptions{MCP, MCPServers}` → заголовки `X-Edy-MCP`, `X-Edy-MCP-Servers`. |
| `internal/omniroute/server.go:185-200` | Роутер принимает `X-Edy-MCP`, проверяет адрес и провайдера, собирает `MCPAttach`. |
| `internal/omniroute/cli.go:85-94` | Вставка MCP-флагов в командную строку CLI-утилиты. |
| `internal/omniroute/caps.go:12, 86-90` | Тег возможности `mcp`; ставится автоматически только для CLI-провайдеров, которые `SupportsMCP`. |
| `internal/app/app.go:128, 170, 192` | Подключение: `h.Tasks.MCP = h.MCPBridge()`, `mux.Handle("/mcp", h.MCP())`, `CloseMCP()` при остановке. |
| `internal/api/api.go:82, 124-127` | `GET /api/info` отдаёт `mcp_url`, `token`, `edy_path` для экрана «Подключение». |
| `internal/api/projects.go:165-173` | `knownProject` — allow-list проектов. |
| `internal/api/projects.go:383-399, 485-488` | Каталог для Оркестратора; `PlanMCP` — какие серверы выданы задаче. |
| `internal/orchestrator/orchestrator.go:301-337` | Строка плана `MCP: имя1, имя2` и разбор `<new_mcp>…</new_mcp>`-предложений. |
| `internal/ui/static/index.html:1388-1410` | Генерация команд подключения для пользователя. |

---

## 3. Протокол: `internal/mcp/mcp.go`

### 3.1 Версии и методы

`protocolVersions = ["2025-06-18", "2025-03-26", "2024-11-05"]` (`mcp.go:26`). На `initialize`
сервер отвечает версией клиента, если знает её, иначе — первой (предпочтительной) (`mcp.go:111-127`).

Поддерживаемые методы (`dispatch`, `mcp.go:109-170`):

| Метод | Ответ |
|---|---|
| `initialize` | `protocolVersion`, `capabilities: {tools:{}, prompts:{}}`, `serverInfo: {name:"edy", title:"Edy", version}`, `instructions` (`mcp.go:172-175`). `version` — `Server.Version` или `"dev"`. **Внимание:** ни `api`, ни `cmd/edy` поле `Version` не заполняют, поэтому всегда `"dev"`. |
| `ping` | `{}` |
| `tools/list` | `{tools:[…]}` — см. раздел 4. Пагинации (`cursor`) нет. |
| `tools/call` | `{content:[{type:"text", text}], isError}`. Ошибки инструментов — это **не** JSON-RPC ошибки, а `isError:true`. JSON-RPC ошибка `-32602` только если нет `name`. |
| `prompts/list` | Скиллы как MCP-промпты: `{prompts:[{name, description}]}` (с учётом фильтра `Skills`). |
| `prompts/get` | `{description, messages:[{role:"user", content:{type:"text", text: тело скилла}}]}`; нет скилла → `-32602`. |
| `resources/list` | Всегда `{resources:[]}` (хотя `resources` в capabilities не объявлены). |
| прочее | `-32601 неизвестный метод …` |

Коды ошибок (`mcp.go:73-78`): `-32700` (не JSON), `-32600` (не `jsonrpc:"2.0"` или нет `method`),
`-32601`, `-32602`. Мост `cmd/edy` дополнительно использует `-32603` (`cmd/edy/mcp.go:159-165`).

### 3.2 `Handle` — правила обработки сообщения (`mcp.go:82-102`)

1. Невалидный JSON → ответ с `id:null`, код `-32700`.
2. `jsonrpc != "2.0"` или пустой `method`: если `id` нет — это ответ клиента на наш запрос
   (сервер запросов не шлёт), возвращается `nil`; если `id` есть — `-32600`.
3. Нет `id` → уведомление (`notifications/initialized`, `notifications/cancelled`…) → `nil`, не отвечаем.
4. Иначе `dispatch`; если результат `nil` и ошибки нет — `result: {}`.

Пакетные запросы (JSON-массив) **не поддерживаются**: массив не разбирается в `request` →
`-32700`. В 2025-06-18 батчи из спецификации убраны, в 2025-03-26 они были — см. раздел 13.

### 3.3 Пример диалога

Запросы (одно сообщение на строку в stdio, одно тело POST в HTTP):

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1"}}}
```

Ответ (ключи отсортированы — так их сериализует `encoding/json` для `map`):

```json
{"jsonrpc":"2.0","id":1,"result":{
  "capabilities":{"prompts":{},"tools":{}},
  "instructions":"Edy — инструменты для работы с проектом пользователя.\nНачинайте с project_skeleton, ищите код через search_code, правьте через replace_in_file и write_file.\nКаждая правка сохраняется снимком, пользователь может её откатить. После правок вызовите run_checks.\nЕсли в list_skills есть подходящий скилл, загрузите его через use_skill и следуйте ему.",
  "protocolVersion":"2025-06-18",
  "serverInfo":{"name":"edy","title":"Edy","version":"dev"}}}
```

```json
{"jsonrpc":"2.0","method":"notifications/initialized"}
```
→ ответа нет (по HTTP — `202 Accepted` с пустым телом).

```json
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
```
→ (сокращено)
```json
{"jsonrpc":"2.0","id":2,"result":{"tools":[
  {"name":"project_skeleton","description":"Скелет проекта: …","inputSchema":{"type":"object","properties":{}},"annotations":{"readOnlyHint":true}},
  {"name":"replace_in_file","description":"Заменить в файле фрагмент old на new. …","inputSchema":{"type":"object","properties":{"path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"}},"required":["path","old","new"]},"annotations":{"readOnlyHint":false}},
  …]}}
```

```json
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"replace_in_file","arguments":{"path":"calc.go","old":"a + b","new":"b + a"}}}
```
→
```json
{"jsonrpc":"2.0","id":3,"result":{"content":[{"text":"заменено в calc.go","type":"text"}],"isError":false}}
```
и в Shadow Git появляется коммит `MCP: replace_in_file calc.go`.

Ошибка инструмента:
```json
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"../secret"}}}
```
→
```json
{"jsonrpc":"2.0","id":4,"result":{"content":[{"text":"Ошибка: путь \"../secret\" выходит за пределы проекта","type":"text"}],"isError":true}}
```

Во время работы задачи над тем же проектом:
```json
{"jsonrpc":"2.0","id":5,"result":{"content":[{"text":"Ошибка: над проектом сейчас работает задача Edy — правки через MCP временно запрещены.","type":"text"}],"isError":true}}
```

### 3.4 Тип `Server` (`mcp.go:29-51`)

Один `Server` обслуживает **один проект**. Поля:

| Поле | Смысл |
|---|---|
| `Root string` | Корень проекта; все пути инструментов — относительно него. |
| `Home string` | Папка данных Edy; общие скиллы лежат в `Home/skills`. |
| `Version string` | Версия для `serverInfo` (сейчас нигде не задаётся → `"dev"`). |
| `Skills []string` | Фильтр скиллов. `nil` — все скиллы проекта и общие; `[]string{}` — ни одного; список — только эти имена. Задачи передают сюда скиллы, выданные Оркестратором. |
| `Memory agent.Memory` | Память проекта. `nil` → инструментов `project_memory`/`remember` нет в `tools/list`. |
| `Code agent.CodeSearch` | Поиск по смыслу для `search_code`; `nil` → только BM25 по словам. |
| `Busy func() bool` | `true` → правящие инструменты отклоняются (над проектом работает задача Edy). |
| `OnChange func(step)` | После каждой успешной правки и снимка; `step` = `"MCP: <tool> <path>"`. |
| `OnCall func(tool, note, isErr)` | После каждого `tools/call` (для журнала). `note` — `"<path/query/name> → <первая строка ответа до 160 символов>"` (`note`, `mcp.go:333-349`). |
| `mu sync.Mutex` | Правки и снимки по очереди (в пределах одного `Server`). |
| `shadow *agent.Shadow` | Ленивое открытие Shadow Git при первой правке (`snapshot`, `mcp.go:291-301`). |

### 3.5 Логика `call` (`mcp.go:236-289`)

```
call(name, args):
  "list_skills"  → s.skills() → skills.Catalog(...) или пояснение «скиллов нет / не выданы»
  "run_checks"   → agent.DetectHooks(Root) → agent.RunHooks → отчёт; isError = !ok
                   (Busy НЕ проверяется, mu НЕ берётся)
  иначе:
    writes := !readOnly[name] && !noFiles[name]
    if writes:
        if Busy() → "Ошибка: над проектом сейчас работает задача Edy…", isError
        mu.Lock()
    ws := agent.Workspace{Root, Skills: s.skills(), Memory, Code}
    out, changed, err := ws.Call(name, args)
    err → "Ошибка: "+err, isError
    changed → snapshot("MCP: <name> <path>") → OnChange(step)
              (ошибка снимка не валит вызов: в ответ дописывается «(снимок не сохранён: …)»)
```

- `readOnly` (`mcp.go:209`): `project_skeleton, search_code, list_files, read_file, list_skills, use_skill, project_memory`.
- `noFiles` (`mcp.go:213`): `remember` — пишет в память, а не в файлы: ни блокировки, ни снимка,
  работает даже при `Busy` (проверено `TestMemoryTools`).
- Всё остальное считается «правящим», **в том числе неизвестные имена** (см. раздел 13).
- Снимок делается **после** правки (`git add -A && git commit --allow-empty`). Отдельного
  «снимка до» у MCP нет — см. раздел 13.

### 3.6 Транспорт stdio (`ServeStdio`, `mcp.go:313-330`)

Читает построчно (буфер `bufio.Reader` 1 МиБ, но `ReadBytes` строку не ограничивает), пустые строки
пропускает, на каждую непустую вызывает `Handle`, ответ пишет одной строкой с `\n`. На EOF —
`nil`. Логи в stdout писать нельзя (сломает протокол) — `cmd/edy/mcp.go` пишет в stderr.

---

## 4. Справочник инструментов

Список строится в `Server.tools()` (`mcp.go:215-233`): берутся `agent.Workspace.Tools()`
(`workspace.go:94-124`) без `finish`, плюс `run_checks` и `list_skills`. `use_skill` есть всегда —
`tools()` передаёт в `Workspace` фиктивный скилл (`Skills: []skills.Skill{{}}`), «потому что скиллы
могут появиться, пока клиент подключён». Каждому инструменту ставится аннотация
`readOnlyHint` (других аннотаций — `destructiveHint`, `idempotentHint`, `title` — нет).

Все пути — **относительные** от корня проекта. Запрещено: абсолютные пути, `..` наружу, первый
компонент `.git` или `.edy`, симлинки, ведущие за пределы проекта (`workspace.go:54-91`).

| Инструмент | Параметры (JSON Schema) | Поведение | readOnlyHint | Блокируется `Busy` | Снимок |
|---|---|---|---|---|---|
| `project_skeleton` | `{}` | `codeindex.Build` + `Skeleton(30000)`: файлы, типы, функции/классы с номерами строк без тел. Индекс строится заново на каждый вызов. | true | нет | нет |
| `search_code` | `query: string` (обяз.), `limit: integer` (по умолч. 5; ≤0 или >20 → 5) | BM25 по индексу; если задан `Code` — гибридный поиск (слова + смысл), возможна пометка `(…)` в начале ответа. Пустой `query` → ошибка. | true | нет | нет |
| `list_files` | `path: string` (необяз., по умолч. корень) | Рекурсивно, без `.git .edy node_modules vendor __pycache__ .venv venv`, не более 500 файлов, сортировка. | true | нет | нет |
| `read_file` | `path: string` (обяз.) | Файл целиком; больше 200 КиБ — обрезка с пометкой `…(файл обрезан)`. Двоичные файлы не фильтруются. | true | нет | нет |
| `write_file` | `path`, `content` (обяз.) | Создаёт папки, пишет файл `0644`, перезаписывает целиком. | false | **да** | да |
| `replace_in_file` | `path`, `old`, `new` (обяз.) | `old` должен встречаться ровно один раз; пустой `old`, 0 или >1 вхождений → ошибка. | false | **да** | да |
| `delete_file` | `path` (обяз.) | `os.Remove` (папку — только пустую). | false | **да** | да |
| `project_memory` | `query: string` (обяз.) | `Memory.Search(query)`. Есть в списке только при `Memory != nil`. | true | нет | нет |
| `remember` | `text: string` (обяз.) | `Memory.Remember(text)`; для внешних клиентов лимит 20 записей на сессию (`mcpapi.go:155`). Есть только при `Memory != nil`. | false | нет (`noFiles`) | нет |
| `use_skill` | `name: string` (обяз.) | Текст скилла `# Скилл <имя>\n\n<тело>`; невыданный/несуществующий → ошибка. | true | нет | нет |
| `list_skills` | `{}` | Каталог скиллов (`skills.Catalog`) с учётом фильтра; иначе пояснение. | true | нет | нет |
| `run_checks` | `{}` | `agent.DetectHooks`: `go vet/go test`, `ruff/pytest` или `.edy/hooks.json`; `isError = !ok`. Нет проверок → текст «Проверки не найдены…», `isError:false`. | false | **нет** | нет |

`finish` (внутренний инструмент агента) в `tools/list` не попадает; вызвать его по MCP нельзя —
`Workspace.Call` его не знает и вернёт «неизвестный инструмент».

Когда какие инструменты видны:

| Режим | `project_memory`/`remember` | `search_code` по смыслу | Фильтр скиллов | `Busy` |
|---|---|---|---|---|
| `edy mcp`, Edy не запущен (автономный) | нет | нет | все | нет (`nil`) |
| HTTP с сессией (внешний клиент) | да (`memory.Access`, Source `mcp:<id>`) | да, если включён `h.Semantic` | все | `Tasks.Running(project)` |
| HTTP без `Mcp-Session-Id` и без `initialize` | нет | нет | все | `Tasks.Running(project)` |
| HTTP с `bind=` (исполнитель задачи) | память задачи | поиск задачи | только выданные (`[]` если ни одного) | **нет** |

---

## 5. HTTP-транспорт `/mcp` (`internal/api/mcpapi.go`)

### 5.1 Маршрут и проверки

`mux.Handle("/mcp", h.MCP())` (`app.go:170`) — обработчик смонтирован **мимо** `Handler.ServeHTTP`,
поэтому проверку токена делает сам `serveMCP` (`mcpapi.go:52-141`):

1. `Authorization: Bearer <токен OmniRoute>` — сравнение `subtle.ConstantTimeCompare` с `h.Token`.
   Нет/не тот → `401 {"error":"нужен токен OmniRoute"}`. Это тот же токен, что у `/v1` и `/api`
   (хранится в настройке `router_token`, копия — в `$EDY_HOME/router.token` с правами 0600,
   `app.go:60-75`; показать: `edy token`).
2. Метод: `POST` — основной; `DELETE` — закрыть сессию (`Mcp-Session-Id`), всегда `204`;
   прочие (в том числе `GET` для SSE-потока) → `405`, `Allow: POST, DELETE`. Edy не шлёт
   серверных уведомлений, поэтому SSE не нужен; ответ на `POST` всегда `application/json`.
3. `?project=<абсолютный путь>` обязателен (`400`), и путь должен **точно** совпадать с одним из
   проектов Edy (`knownProject`, `projects.go:165-173`) — иначе `404 «проект не найден — добавьте
   его в разделе «Проект»»`. Сравнение строковое: без нормализации, симлинков и `/` на конце.
4. Тело читается до 8 МиБ (`io.LimitReader`, `mcpapi.go:83`); длиннее — молча обрезается
   (получится `-32700`).
5. `?bind=<key>` (только у исполнителей задач): ключ ищется в `h.mcpB`; нет или другой проект →
   `403 «привязка MCP к задаче устарела»`.
6. Выбор сессии:
   - `method == "initialize"` → **всегда новая** сессия (`newMCPSession`), заголовок ответа
     `Mcp-Session-Id: mcp-<16 hex>`.
   - есть `Mcp-Session-Id` → ищется в `h.mcpS`, обновляется `used`; нет или проект не совпадает →
     `404 «сессия MCP не найдена — переподключитесь»` (по спецификации клиент должен заново
     сделать `initialize`).
   - нет ни того, ни другого → временный `mcp.Server` без журнала и памяти (клиент «без сессий»).
7. `Handle` вернул `nil` (уведомление) → `202 Accepted`; иначе `200` + JSON.

Заголовок `MCP-Protocol-Version` (обязательный для клиентов 2025-06-18) и `Origin` не проверяются.

### 5.2 Сессии внешних клиентов и журнал «Агенты»

`newMCPSession` (`mcpapi.go:178-209`):

- `id = "mcp-" + randomHex(8)`;
- если нет привязки — создаётся запись агента `store.Agent{ID: id, Role: store.RoleMCP ("mcp"),
  Model: "<clientInfo.name> <clientInfo.version>" или "MCP-клиент", Project, Goal: "Внешний агент
  работает с проектом через MCP"}` — она видна в окне «Агенты»;
- `mcpServer(project, agentID, bind)` (`mcpapi.go:143-176`) настраивает сервер:
  - `Memory = memory.Access{Source: "mcp:"+agentID, Limit: 20}`, при наличии `h.Semantic` —
    `Ranker` по смыслу и `Code = semantic.Code`;
  - `OnCall` → `Store.AddAgentEvent(agentID, "tool"|"error", "<tool> <note>")`;
  - `OnChange` → `AddAgentEvent(agentID, "checkpoint", "Снимок — MCP: …")`;
  - `Busy = h.Tasks.Running(project)`.
- Если сессий уже `maxMCPSessions = 64` (`mcpapi.go:27`) — самые давно не использованные
  вытесняются (сортировка по `used`), агент отмечается `AgentDone` «Сессия закрыта».

Закрытие: `DELETE /mcp` («Клиент отключился»), вытеснение («Сессия закрыта»), `CloseMCP()` при
остановке Edy («Edy закрыт», `mcpapi.go:255-262`, вызывается из `app.go:192`). При перезапуске
Edy сессии в памяти теряются; агенты прошлого запуска помечаются прерванными
(`InterruptRunningAgents`, `app.go`).

### 5.3 Привязки задач (`MCPBridge`, `mcpapi.go:217-252`)

`h.MCPBridge()` реализует `agent.MCPBridge` (`task.go:71-79`):

- `Supports(model)` — `"провайдер/модель"`: провайдер существует, `Kind == cli` и у него есть тег
  `mcp` (`omniroute.HasCap`). Тег автоматически включён только у CLI-провайдеров, чья команда
  `SupportsMCP` (сейчас — `claude`), и пользователь может его снять (`PUT /api/providers/{name}/caps`).
- `Bind(root, ev)`:
  - `key = randomHex(16)` (32 hex-символа), `h.mcpB[key] = {project: root, ev}`;
  - возвращает URL `http://<h.Addr>/mcp?project=<root>&bind=<key>` и `release()`.
  - `release()` удаляет ключ и **все** сессии с `project == root && agentID == ""`
    (то есть сессии привязок этого корня).
- Сервер привязанной сессии: `OnCall/OnChange/Skills/Memory/Code` берутся из `ev`
  (`mcpapi.go:146-153`); `Busy` **не** ставится — задача сама держит проект, правки её
  исполнителя разрешены. Отдельная запись в «Агентах» не создаётся: события идут в ленту задачи
  (`task.go:349-368`: `tool`/`error` и `checkpoint` «Снимок №N — write_file sub.go»).

### 5.4 Жизненный цикл (ASCII)

```
ВНЕШНИЙ КЛИЕНТ (Claude Code / Cursor / edy mcp-мост)

  POST /mcp?project=P  {initialize}            ─► 200 + Mcp-Session-Id: mcp-XXXX
        │                                          Store.CreateAgent(role=mcp, state=running)
        ▼
  POST … {notifications/initialized} + Session ─► 202
        ▼
  POST … {tools/call} + Session                ─► 200; AddAgentEvent(tool|error|checkpoint)
        │      (если Tasks.Running(P) — правки → isError «задача Edy»)
        ▼
  ┌─ DELETE /mcp + Session ─────────► 204, FinishAgent(done, «Клиент отключился»)
  ├─ 65-я сессия (LRU по used) ─────► FinishAgent(done, «Сессия закрыта»)
  ├─ Edy закрывается ───────────────► FinishAgent(done, «Edy закрыт»)
  └─ Edy перезапущен ───────────────► следующий запрос: 404 → клиент должен initialize заново


ИСПОЛНИТЕЛЬ ЗАДАЧИ (Claude Code, запущенный Edy)

  agent.Task.attemptMCP
     │ url, release := MCPBridge.Bind(root, MCPEvents{Skills, Memory, Code, OnCall, OnChange})
     │       h.mcpB[KEY] = {root, ev}
     │ ctx += CallOptions{MCP: url, MCPServers: Extra}
     ▼
  LLM.Chat ─► OmniRoute (in-process) заголовки X-Edy-MCP=url, X-Edy-MCP-Servers=docs,gh
     │          server.go: checkMCPURL, SupportsMCP, extraMCP → MCPAttach{URL, Token, Extra}
     ▼
  cli.go: claude --mcp-config /tmp/edy-mcp-NNN.json --strict-mcp-config
                 --allowedTools mcp__edy mcp__docs --disallowedTools Bash Edit Write MultiEdit NotebookEdit
                 <остальные аргументы провайдера>
     │
     │   claude ── POST /mcp?project=root&bind=KEY {initialize} ─► сессия (agentID=""), без Busy
     │   claude ── tools/call write_file … ─► правка + снимок → лента задачи «Снимок №N»
     ▼
  процесс claude завершился → cleanup(): tmp.json удалён
     │
  Edy запускает RunHooks; провал → новая попытка (тот же Bind/URL, новый процесс claude)
     ▼
  defer release(): delete mcpB[KEY]; удалить все сессии с project==root && agentID==""
     │  (дальнейшие запросы с этим KEY → 403 «привязка устарела»)
```

---

## 6. Режим stdio: `edy mcp` (`cmd/edy/mcp.go`)

```
edy mcp [-project DIR] [-addr 127.0.0.1:11435]
```

`runMCP` (`mcp.go:28-59`):

1. `-project` (по умолчанию `.`) → `filepath.Abs`, должна существовать папка.
2. `app.Running(addr)` — `GET http://addr/healthz` с таймаутом 1 с.
3. Если Edy запущен и читается `$EDY_HOME/router.token`:
   - `bridge.register` — `POST /api/projects {"path": root}`: папка **добавляется** в проекты Edy
     (или поднимается наверх списка), путь берётся из ответа (нормализованный `filepath.Clean`).
   - `bridge.serve` — каждую строку stdin пересылает `POST /mcp?project=…` с токеном,
     `Accept: application/json, text/event-stream` и `Mcp-Session-Id` (запоминает из ответа).
     `202` → ничего не пишет; `200` → тело в stdout; иначе — JSON-RPC ошибка `-32603`
     «Edy: HTTP <код> <текст>» (для уведомлений — ничего). HTTP-клиент с таймаутом 15 минут
     (долгие `run_checks`).
   - На EOF stdin — `DELETE /mcp` (сессия закрывается, агент в журнале → done).
   - В stderr: «edy mcp: работаю через запущенный Edy …».
4. Иначе (Edy не запущен, нет токена, `register` не удался) — автономный `mcp.Server{Root, Home}`
   прямо в этом процессе: снимки делаются, но журнала, памяти, поиска по смыслу и блокировки
   `Busy` нет.

Важно: `cmd/edy/main.go:69-73` открывает базу (`app.OpenStore` → `vault.FromKeychain`) **до**
разбора команды — значит `edy mcp` требует доступ к Keychain даже в автономном режиме, хотя база
ему не нужна. На Linux без secret-service команда падает («dbus-launch не найден»); на macOS при
первом запуске из чужого процесса может появиться системный запрос доступа к связке ключей.

---

## 7. Как Claude Code получает MCP (`internal/omniroute/climcp.go`)

### 7.1 Путь от задачи до командной строки

1. `agent.Task.Run` для каждой модели: `t.MCP != nil && t.MCP.Supports(model)` → `attemptMCP`
   (`task.go:182-187`); иначе — обычный цикл с эмуляцией инструментов в промпте, и если задаче
   выданы внешние серверы, в ленту пишется «… не умеет MCP: серверы … не выданы».
2. `attemptMCP` (`task.go:343-411`): `Bind`, промпт `mcpPrompt` + каталог скиллов + заметка о
   памяти + (если есть `Extra`) «тебе также выданы MCP-серверы: …, правь файлы проекта только
   через edy»; `WithCallOptions{Workdir, MCP: url, MCPServers: t.Extra}`.
3. `agent.OmniRoute.Chat` (`llm.go:116-124`) ставит заголовки `X-Edy-Workdir`, `X-Edy-MCP`
   (константа `omniroute.MCPHeader`) и `X-Edy-MCP-Servers` (`omniroute.MCPServersHeader`,
   имена через запятую). Запрос идёт **в процессе** (`agent.InProcess(router)`).
4. `omniroute/server.go:185-200`:
   - `checkMCPURL` (`climcp.go:98-108`): только `http://`, путь ровно `/mcp`, хост
     `127.0.0.1 | localhost | ::1` — токен Edy не должен уйти на чужой сервер;
   - провайдер должен быть CLI и `SupportsMCP(command)`, иначе `400`;
   - `extraMCP` (`climcp.go:111-135`): имена из заголовка ищутся среди **включённых** серверов
     каталога (расшифрованных); неизвестное или выключенное имя → `400`;
   - `call.MCP = &MCPAttach{URL, Token: токен роутера, Extra}`.
5. `cli.go:85-94`: по `filepath.Base(args[0])` выбирается функция из `mcpCapable`
   (`climcp.go:38-40`: только `"claude": claudeMCP`); её аргументы вставляются **сразу после имени
   программы**, `cleanup` — `defer`.

### 7.2 `claudeMCP` (`climcp.go:55-94`)

Создаёт временный файл `os.CreateTemp("", "edy-mcp-*.json")`, `chmod 0600`, содержимое:

```json
{"mcpServers":{
  "edy":  {"type":"http","url":"http://127.0.0.1:11435/mcp?project=%2FUsers%2F…&bind=<KEY>",
           "headers":{"Authorization":"Bearer <EDY_TOKEN>"}},
  "docs": {"type":"http","url":"https://docs.example/mcp","headers":{"Authorization":"Bearer <SECRET>"}},
  "playwright": {"type":"stdio","command":"npx","args":["@playwright/mcp@latest"],"env":null}
}}
```

Аргументы (пример итоговой командной строки для провайдера `claude -p --model {model} {prompt}`):

```
claude --mcp-config /tmp/edy-mcp-123.json --strict-mcp-config \
       --allowedTools mcp__edy mcp__docs mcp__playwright \
       --disallowedTools Bash Edit Write MultiEdit NotebookEdit \
       -p --model sonnet "<промпт>"
```

- `--strict-mcp-config` — Claude Code игнорирует MCP-серверы из своих настроек пользователя/проекта
  и берёт только этот файл.
- `--allowedTools mcp__edy …` — разрешение (без запроса подтверждения) на все инструменты сервера
  `edy` и каждого выданного внешнего (`mcp__<имя>`).
- `--disallowedTools Bash Edit Write MultiEdit NotebookEdit` — у Claude Code отняты терминал и
  собственная правка файлов: все изменения идут через Edy (снимки), тесты — через `run_checks`.
  `Read`, `Glob`, `Grep`, `WebFetch`, `WebSearch`, `Task`, `TodoWrite` **не** запрещены.
- Файл удаляется после завершения процесса (`cleanup`); e2e-тест это проверяет.
- Тип внешнего сервера не `http`/`stdio` → сервер молча пропускается (`climcp.go:68-70`).

### 7.3 `SupportsMCP` и тег `mcp`

`SupportsMCP(command)` (`climcp.go:43-49`) — `filepath.Base(command[0])` есть в `mcpCapable`.
Используется: при выдаче тега `mcp` (`caps.go:86-90`), в `GET /api/providers` (`CanMCP`,
`api.go:359-360`) и при проверке запроса (`server.go:190`). Чтобы добавить новую утилиту
(Gemini CLI, Codex…), нужно написать функцию типа `mcpFlags` и добавить её в `mcpCapable` —
больше ничего менять не надо.

---

## 8. Каталог внешних MCP-серверов

### 8.1 Хранение (`internal/store/mcpservers.go`)

Таблица (`mcpservers.go:17-25` + миграция `store.go:70` добавляет `account`):

```sql
CREATE TABLE IF NOT EXISTS mcp_servers (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE,
  description TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
  config BLOB NOT NULL);
-- + ALTER TABLE mcp_servers ADD COLUMN account TEXT NOT NULL DEFAULT ''
```

- В открытом виде: `name`, `description`, `account`, `enabled`.
- `config` — JSON `MCPConfig{type, url, headers, command, args, env}`, **зашифрованный**
  `vault.Seal` (AES-256-GCM, мастер-ключ в macOS Keychain, сервис `dev.edy.ide`). Зашифрованы
  все поля конфигурации, не только секреты (URL и команда тоже).
- `Validate` (`mcpservers.go:54-74`): имя `^[a-z0-9][a-z0-9_-]{0,31}$`, не `edy` (занято
  встроенным сервером — иначе конфликт ключа в `mcpServers` и `mcp__edy`); `http` — URL с
  `https://` или `http://` (сообщение об ошибке говорит только про https); `stdio` — непустая команда.
- `SaveMCPServer` — upsert по имени (`ON CONFLICT(name) DO UPDATE`).
- `MCPServers()` — **расшифрованный** список, по имени. Ошибка расшифровки любой строки валит
  весь список.

«Приватный» сервер — это просто запись с секретом в `headers` (например,
`Authorization: Bearer …`) или в `env` (например, `GITHUB_TOKEN=…`), плюс поле `account`
(«GitHub: работа»), чтобы Оркестратор различал несколько аккаунтов одного сервиса.

### 8.2 HTTP API (`internal/api/mcpcatalog.go`)

Маршруты (`mcpcatalog.go:60-65`, все под токеном `/api`):

| Маршрут | Тело / ответ |
|---|---|
| `GET /api/mcp-servers` | `{servers:[mcpServerJSON], presets:[mcpPreset]}` |
| `POST /api/mcp-servers` | `{name, description, account, type, url, command, args, headers, env}` — `args` по одному в строке, `headers` строки «Имя: значение», `env` строки «ИМЯ=значение». Пустое `description` → `400` (по нему Оркестратор решает, выдавать ли). Ответ `201 {"name"}`. Сохраняет всегда `enabled: true`. |
| `PUT /api/mcp-servers/{name}/enabled` | `{"enabled": bool}` → `{"ok":true}`; нет сервера → `404`. |
| `DELETE /api/mcp-servers/{name}` | `{"ok":true}`; нет → `404`. |

`mcpServerJSON` (`mcpcatalog.go:47-58`) отдаёт `headers` и `env` **только именами ключей**
(`keysOf`), значения не уходят в интерфейс — проверено в коде (`listMCPServers`,
`mcpcatalog.go:75-87`) и тестом `TestMCPCatalogHidesSecrets`. Но `url`, `command` и `args`
отдаются как есть — секрет в query-строке URL или в аргументе команды будет виден (раздел 10).

Пресеты (`mcpcatalog.go:30-43`): `playwright` (`npx @playwright/mcp@latest`), `context7`
(`npx -y @upstash/context7-mcp`), `fetch` (`uvx mcp-server-fetch`), `github`
(`https://api.githubcopilot.com/mcp/`, нужен заголовок `Authorization: Bearer <токен GitHub>`).
Поле `Secret` пресета — подсказка формы, какой заголовок/переменную заполнить.

### 8.3 Как серверы попадают к исполнителю

1. Оркестратор видит включённые серверы (`orchestratorServers`, `projects.go:383-399`: имя,
   описание, аккаунт — без конфигурации) и, если основной исполнитель умеет MCP, пишет в план
   строку `MCP: имя1, имя2` (`mcpLineRe`, `orchestrator.go:301`). Иначе инструкция «строку MCP не пиши»
   (`orchestrator.go:206`).
2. Оркестратор может предложить новый сервер блоком `<new_mcp>{json}</new_mcp>` (`orchestrator.go:304,
   350-356`) — интерфейс показывает карточку-предложение (`index.html:2013`), сохраняет пользователь.
3. `POST /api/tasks` с планом: `opts.MCP = orchestrator.PlanMCP(plan, servers)` — только известные
   имена (`projects.go:485-488`); постоянный агент команды добавляет свои (`union`, `projects.go:523`).
4. `tasks.go:1018` → `agent.Task.Extra`; дальше раздел 7: имена в заголовке
   `X-Edy-MCP-Servers`, конфигурации с секретами — только внутри процесса Edy и во временном
   файле `0600` для Claude Code.

---

## 9. Подключение внешних клиентов к MCP Edy

Экран «Подключение» в интерфейсе генерирует эти команды сам (`index.html:1388-1410`) из
`GET /api/info` (`mcp_url`, `token`, `edy_path`). Ниже — то же вручную. `<EDY_TOKEN>` —
вывод `edy token` (или файл `$EDY_HOME/router.token`); `/Users/me/projects/app` — путь к проекту,
**в точности** как он записан в списке проектов Edy.

Где `edy`: в собранном Edy.app CLI лежит в `Edy.app/Contents/Resources/bin/edy` (`edyPath`,
`api.go:131-145`). `$EDY_HOME` по умолчанию `~/Library/Application Support/Edy`.

### 9.1 Claude Code через HTTP (нужен запущенный Edy)

```bash
claude mcp add --transport http edy \
  'http://127.0.0.1:11435/mcp?project=%2FUsers%2Fme%2Fprojects%2Fapp' \
  --header 'Authorization: Bearer <EDY_TOKEN>'
```

Путь в `project` должен быть URL-кодирован (интерфейс использует `encodeURIComponent`).
Плюсы: журнал в окне «Агенты», память проекта, поиск по смыслу, блокировка во время задач.

### 9.2 Claude Code через stdio (работает и без окна Edy)

```bash
claude mcp add edy -- '/Applications/Edy.app/Contents/Resources/bin/edy' mcp -project '/Users/me/projects/app'
```

Если Edy запущен — мост (как 9.1, плюс автодобавление проекта в список); если нет — автономный
сервер (без журнала и памяти).

### 9.3 Cursor, Gemini CLI, Claude Desktop (JSON, stdio)

Cursor: `.cursor/mcp.json` в проекте или `~/.cursor/mcp.json`; Gemini CLI: `~/.gemini/settings.json`;
Claude Desktop: `claude_desktop_config.json`.

```json
{
  "mcpServers": {
    "edy": {
      "command": "/Applications/Edy.app/Contents/Resources/bin/edy",
      "args": ["mcp", "-project", "/Users/me/projects/app"]
    }
  }
}
```

### 9.4 Cursor / другие клиенты через HTTP

Формат зависит от клиента; типичный (Cursor):

```json
{
  "mcpServers": {
    "edy": {
      "url": "http://127.0.0.1:11435/mcp?project=%2FUsers%2Fme%2Fprojects%2Fapp",
      "headers": { "Authorization": "Bearer <EDY_TOKEN>" }
    }
  }
}
```

Для Claude Code в формате `.mcp.json` добавьте `"type": "http"`.

### 9.5 Проверка curl'ом

```bash
TOKEN=$(cat "$HOME/Library/Application Support/Edy/router.token")
P='%2FUsers%2Fme%2Fprojects%2Fapp'
curl -si -X POST "http://127.0.0.1:11435/mcp?project=$P" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"curl","version":"1"}}}'
# → HTTP/1.1 200, заголовок Mcp-Session-Id: mcp-…
curl -s -X POST "http://127.0.0.1:11435/mcp?project=$P" \
  -H "Authorization: Bearer $TOKEN" -H 'Mcp-Session-Id: mcp-…' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
```

Типичные ошибки: `401` — не тот токен; `404 проект не найден` — путь не в списке или записан
иначе (симлинк, `/` в конце, не URL-кодирован `&`); `404 сессия MCP не найдена` — Edy
перезапускали, нужно переподключить клиент; `405` — клиент пытается открыть `GET`-поток (это
нормально, Claude Code переживает).

---

## 10. Модель безопасности

| Механизм | Где | Что защищает |
|---|---|---|
| Bearer-токен OmniRoute | `mcpapi.go:53-57` | Весь `/mcp`. Один токен на всё (`/v1`, `/api`, `/mcp`), сравнение за постоянное время. Сервер слушает `127.0.0.1:11435` по умолчанию (`omniroute.DefaultAddr`). |
| Allow-list проектов | `knownProject`, `projects.go:165-173` | По MCP можно работать только с папками, добавленными в Edy (до 10 последних, `projects.go:137-141`). Для `bind=` проверяется дополнительно совпадение проекта привязки. |
| Песочница путей | `Workspace.resolve`, `workspace.go:54-91` | Нет абсолютных путей, `..`, `.git`, `.edy`, симлинков наружу. |
| Блокировка `Busy` | `mcp.go:266-273` | Внешний клиент не правит проект, пока над ним работает задача Edy (кроме `run_checks` и `remember`). |
| Снимки | `mcp.go:280-287` | Любую правку по MCP можно откатить в интерфейсе (если до неё был снимок — см. раздел 13). |
| Ключ привязки | `mcpapi.go:232-252` | 128 бит случайности; живёт только пока идёт попытка исполнителя; после `release` → `403`. |
| `checkMCPURL` | `climcp.go:98-108` | Токен Edy в `--mcp-config` уходит только на localhost `/mcp`. |
| Конфиг Claude Code | `climcp.go:75-93` | Файл `0600` в `$TMPDIR`, удаляется после процесса. Содержит токен Edy **и секреты внешних серверов** в открытом виде на время работы. |
| `--disallowedTools` | `climcp.go:92` | Исполнитель не правит файлы и не запускает команды мимо Edy. |
| Каталог | `mcpservers.go:77-91`, `mcpcatalog.go:47-87` | Конфигурация шифруется AES-256-GCM; API отдаёт только имена заголовков/переменных. Оркестратор видит только имя/описание/аккаунт. В заголовке `X-Edy-MCP-Servers` — только имена. |

Проверено по коду: значения `Headers`/`Env` не возвращаются ни одним маршрутом API
(`listMCPServers` — единственный, кто читает `MCPServers()` для ответа; `orchestratorServers`
берёт только имя/описание/аккаунт; `extraMCP` используется только внутри роутера). Секреты
доходят только до командной строки/файла дочернего процесса.

Слабые места:

- `url`, `command`, `args` каталога отдаются в API открыто — токен в URL (`?key=…`) или в
  аргументе будет виден в интерфейсе.
- `GET /api/info` отдаёт сам токен (под тем же токеном — круг замкнут, но любой, кто его знает,
  получает всё).
- `Origin`/`Host` у `/mcp` не проверяются: защита от DNS-rebinding держится только на токене.
- `Read`/`Grep`/`Glob` Claude Code не запрещены — исполнитель может читать файлы вне проекта
  средствами самого Claude Code (Edy это не контролирует и не журналирует).
- Внешние stdio-серверы из каталога запускаются Claude Code с правами пользователя — это
  выполнение произвольной команды из каталога (осознанное решение, но стоит помнить).

---

## 11. Справочник объявлений

### `internal/mcp/mcp.go`

| Объявление | Строка | Описание |
|---|---|---|
| `protocolVersions` | 26 | Поддерживаемые версии MCP, первая — предпочтительная. |
| `type Server` | 29 | MCP-сервер одного проекта (поля — раздел 3.4). |
| `func (*Server) Handle(ctx, raw) []byte` | 82 | Обработать одно JSON-RPC сообщение; `nil` для уведомлений. |
| `func (*Server) ServeStdio(ctx, in, out) error` | 313 | Транспорт stdio: построчно. |
| `request`, `rpcError`, `response` | 53, 60, 65 | JSON-RPC структуры (неэкспорт.). |
| `codeParse … codeInvalidParams` | 73-78 | Коды ошибок. |
| `dispatch` | 109 | Маршрутизация по `method`. |
| `instructions` | 172 | Текст `instructions` для `initialize`. |
| `skills()` | 184 | Скиллы проекта+общие с учётом фильтра `Skills`. |
| `tool`, `readOnly`, `noFiles`, `tools()` | 202, 209, 213, 215 | Описание и список инструментов. |
| `call` | 236 | Выполнение инструмента, `Busy`, блокировка, снимок. |
| `snapshot` | 291 | Ленивое `OpenShadow` + `Snapshot`. |
| `pathArg`, `note` | 303, 333 | Извлечь `path`; короткая запись для журнала. |

### `internal/api/mcpapi.go`

| Объявление | Строка | Описание |
|---|---|---|
| `maxMCPSessions = 64` | 27 | Предел сессий в памяти. |
| `type mcpSession` | 31 | Сессия: сервер, id, agentID (пусто у привязок), проект, `used`. |
| `type mcpBinding` | 41 | Привязка задачи: проект + `agent.MCPEvents`. |
| `func (*Handler) MCP() http.Handler` | 48 | Обработчик `/mcp`. |
| `serveMCP` | 52 | Токен, метод, проект, привязка, сессия, `Handle`. |
| `mcpServer` | 143 | Настройка `mcp.Server` для привязки / внешней сессии / без сессии. |
| `newMCPSession` | 178 | Новая сессия + агент в журнале + вытеснение. |
| `finishMCP` | 211 | Пометить агента сессии завершённым. |
| `func (*Handler) MCPBridge() agent.MCPBridge` | 218 | Мост для исполнителей задач. |
| `mcpBridge.Supports` / `.Bind` | 222 / 232 | См. 5.3. |
| `func (*Handler) CloseMCP()` | 255 | Закрыть все сессии при выходе. |
| `randomHex` | 264 | `crypto/rand` → hex. |
| Скиллы: `skillsJSON`, `registerSkillRoutes`, `skillDirs`, `listSkills`, `saveSkill`, `deleteSkill`, `skillScopeDir` | 272-374 | `GET/POST/DELETE /api/skills` (scope `global`/`project`). |

Поля `Handler`, относящиеся к MCP: `mcpM sync.Mutex`, `mcpS map[string]*mcpSession`,
`mcpB map[string]*mcpBinding` (`api.go:74-76`), а также `Token`, `Addr`, `Home`, `Tasks`, `Semantic`.

### `internal/api/mcpcatalog.go`

| Объявление | Строка | Описание |
|---|---|---|
| `mcpPreset`, `mcpPresets` | 16, 30 | Шаблоны популярных серверов. |
| `mcpServerJSON` | 47 | Сервер для интерфейса без значений секретов. |
| `registerMCPCatalogRoutes` | 60 | Регистрация маршрутов (вызов в `api.go:108`). |
| `keysOf` | 67 | Ключи map. |
| `listMCPServers` / `saveMCPServer` / `toggleMCPServer` / `deleteMCPServer` | 75 / 89 / 151 / 169 | CRUD каталога. |
| `pairs` | 133 | Разбор строк «ключ<sep>значение». |

### `internal/store/mcpservers.go`

| Объявление | Строка | Описание |
|---|---|---|
| `MCPHTTP`, `MCPStdio` | 13, 14 | Типы подключения. |
| `mcpSchema` | 17 | DDL таблицы. |
| `type MCPServer` | 31 | Запись каталога (`ID, Name, Description, Account, Enabled` + встроенный `MCPConfig`). |
| `type MCPConfig` | 42 | Как подключиться: `Type, URL, Headers, Command, Args, Env`. |
| `func (MCPServer) Validate() error` | 54 | Проверка имени и конфигурации. |
| `func (*Store) SaveMCPServer(MCPServer) error` | 77 | Upsert с шифрованием. |
| `func (*Store) MCPServers() ([]MCPServer, error)` | 93 | Каталог с расшифровкой. |
| `func (*Store) SetMCPServerEnabled(name, bool) error` | 119 | Вкл/выкл; `ErrNotFound`. |
| `func (*Store) DeleteMCPServer(name) error` | 130 | Удаление; `ErrNotFound`. |

### `internal/omniroute/climcp.go`

| Объявление | Строка | Описание |
|---|---|---|
| `const MCPHeader = "X-Edy-MCP"` | 17 | Адрес MCP Edy для CLI-утилиты. |
| `const MCPServersHeader = "X-Edy-MCP-Servers"` | 22 | Имена внешних серверов через запятую. |
| `type MCPAttach` | 25 | `URL`, `Token`, `Extra []store.MCPServer`. |
| `mcpFlags`, `mcpCapable` | 33, 38 | Функция флагов и реестр утилит (`claude`). |
| `func SupportsMCP(command []string) bool` | 43 | Умеет ли утилита MCP из аргументов. |
| `claudeMCP` | 55 | Конфиг-файл и флаги для Claude Code. |
| `checkMCPURL` | 98 | Только `http://localhost/mcp`. |
| `(*Server).extraMCP` | 111 | Имена → включённые серверы каталога. |

### `cmd/edy/mcp.go`

| Объявление | Строка | Описание |
|---|---|---|
| `runMCP` | 28 | Команда `edy mcp`. |
| `type bridge` | 62 | Мост stdio → HTTP `/mcp`. |
| `register` / `serve` / `forward` / `close` | 70 / 92 / 112 / 146 | Регистрация проекта, цикл, пересылка, `DELETE` сессии. |
| `rpcFailure` | 159 | JSON-RPC ошибка `-32603`. |

### Смежные экспортируемые типы (`internal/agent`)

| Объявление | Где | Описание |
|---|---|---|
| `type MCPBridge interface{Supports; Bind}` | `task.go:72` | Выдача исполнителю адреса MCP, привязанного к задаче. |
| `type MCPEvents struct{Skills, Memory, Code, OnCall, OnChange}` | `task.go:82` | Что видит исполнитель и куда идут события. |
| `Task.MCP`, `Task.Extra` | `task.go:53, 69` | Мост и выданные внешние серверы. |
| `type CallOptions{Workdir, MCP, MCPServers}` | `llm.go:63` | Параметры вызова CLI-провайдера. |
| `store.RoleMCP = "mcp"` | `store/agents.go:14` | Роль агента внешнего MCP-клиента. |
| `omniroute.CapMCP = "mcp"` | `omniroute/caps.go:12` | Тег возможности провайдера. |

---

## 12. Тесты

Запуск всего, что касается MCP:

```bash
go test ./internal/mcp/ ./internal/api/ ./internal/omniroute/ ./internal/store/ ./cmd/edy/ ./internal/app/
# точечно:
go test ./internal/mcp/ -run . -v
go test ./internal/api/ -run 'MCP' -v
go test ./internal/app/ -run TestOrchestratorGivesMCPToClaude -v
```

На коммите `b88fcbf` все проходят (Linux, Go из `go.mod`). Требования: `git` (снимки), `go`
(e2e-тест запускает `go test` как хук). Без них соответствующие тесты пропускаются (`t.Skip`).

| Тест | Файл | Что проверяет |
|---|---|---|
| `TestProtocolAndTools` | `internal/mcp/mcp_test.go:62` | Согласование версии, уведомления без ответа, состав `tools/list` (нет `finish`), `-32601`, `search_code`. |
| `TestEditsAreSnapshottedAndConfined` | `mcp_test.go:91` | Правка → файл изменён, `OnChange`, коммит в `.edy/shadow.git`; `../secret` отклонён; при `Busy` правка запрещена, чтение разрешено. |
| `TestSkillsAsToolsAndPrompts` | `mcp_test.go:127` | `list_skills`, `use_skill`, `prompts/list`, `prompts/get`. |
| `TestStdio` | `mcp_test.go:147` | Построчный stdio, мусорная строка → `-32700`, `ping` → `{}`. |
| `TestTaskSessionSeesOnlyGivenSkills` | `mcp_test.go:164` | Фильтр `Skills` (список / пустой). |
| `TestMemoryTools` | `mcp_test.go:188` | `project_memory`/`remember` появляются с `Memory`; `remember` работает при `Busy`. |
| `TestMCPEndpointJournalsSessions` | `internal/api/mcpapi_test.go:27` | `401` без токена, `404` для неизвестного проекта, `Mcp-Session-Id`, `202` на уведомление, `404` для чужой сессии, запись агента `RoleMCP` с `Model="claude-code 2.1"`, событие `read_file a.txt → привет`, `DELETE` → `AgentDone`. |
| `TestMCPCatalogHidesSecrets` | `internal/api/mcpcatalog_test.go:17` | Обязательное описание, разбор заголовков, отсутствие значений секретов в `GET`, выключение, удаление. |
| `TestOrchestratorAssignsMCPFromCatalog` | `mcpcatalog_test.go:73` | Оркестратор видит каталог при исполнителе с MCP, выдаёт `docs`; при снятом теге — нет; исполнитель без MCP получает предупреждение. |
| `TestMCPServersAreEncrypted` | `internal/store/store_test.go:160` | Валидация и то, что секреты не лежат в БД открытым текстом. |
| `TestMCPAttachRules` | `internal/omniroute/climcp_test.go:11` | `SupportsMCP`, `checkMCPURL`, права `0600`, токен в файле, удаление файла. |
| `TestMCPBridgeToRunningEdy` | `cmd/edy/mcp_test.go:18` | Мост: регистрация проекта, ответы, агент `gemini-cli` закрыт по EOF. |
| `TestMCPWithoutRunningEdy` | `cmd/edy/mcp_test.go:56` | Автономный режим (`-addr 127.0.0.1:1`). |
| `TestOrchestratorGivesMCPToClaude` | `internal/app/mcp_e2e_test.go:101` | Сквозной путь (ниже). |

### Как устроен e2e-тест (`internal/app/mcp_e2e_test.go`)

- `TestMain` (`:22-31`): если `EDY_FAKE_CLAUDE=1`, тестовый бинарник ведёт себя как `claude`
  (`fakeClaude`, `:33-99`).
- Тест создаёт в `PATH` симлинк `claude` → сам тестовый бинарник, провайдера `cc` с командой
  `claude -p --model {model} {prompt}`, сервер каталога `docs` (http, с заголовком-«секретом»),
  поднимает весь Edy (`app.Start` на `127.0.0.1:0`), добавляет git-проект и ставит задачу с
  планом `MCP: docs`.
- Поддельный `claude` проверяет флаги `--strict-mcp-config`, `--allowedTools mcp__edy`,
  `--disallowedTools Bash Edit Write`, наличие `docs` в конфиге и `mcp__docs` в аргументах,
  затем по-настоящему ходит в `/mcp`: `initialize` → `tools/call write_file sub.go`.
- Ожидается: задача `done`, в ленте «получает MCP: Edy, docs (выдал Оркестратор)», «Снимок №1 —
  write_file sub.go», «✓ go test»; файл создан; временных `edy-mcp-*.json` не осталось; записей
  агентов `RoleMCP` нет (сессия привязки не журналируется отдельно).

Чего тесты **не** покрывают: вытеснение 65-й сессии, `403` устаревшей привязки, `405` на `GET`,
поведение моста после перезапуска Edy, внешние stdio-серверы в конфиге Claude Code, эпики
(подзадачи в worktree) с MCP-исполнителем.

---

## 13. Известные ограничения, подозрительные места и ошибки

Найдено чтением кода на `b88fcbf`; пункты с пометкой **(вероятная ошибка)** стоит проверить и
исправить в первую очередь.

1. **(вероятная ошибка) Подзадачи эпика с Claude Code не получат MCP.** Подзадача эпика
   исполняется в worktree `<проект>/.edy/worktrees/<epic>/<…>` (`tasks/epic.go:308`,
   `tasks/worktree.go:34-36`), `Bind` выдаёт URL с `project=<worktree>`, а `serveMCP` вызывает
   `knownProject` **до** проверки `bind` (`mcpapi.go:80-110`). Путь worktree не в списке
   проектов → `404 «проект не найден»`. Claude Code останется без MCP Edy и без своих
   инструментов правки (они запрещены). Исправление: при наличии валидного `bind` пропускать
   `knownProject` (проверять `bind.project == project`), а `knownProject` применять только без
   привязки. Тестом не покрыто.
2. **(вероятная ошибка) Мост `edy mcp` не переживает перезапуск Edy.** После рестарта сессия
   забыта → `404`; `bridge.forward` превращает это в JSON-RPC ошибку и держит устаревший
   `b.session` (`cmd/edy/mcp.go:129-144`). Внешний клиент по stdio не знает, что надо заново
   делать `initialize`, — все вызовы будут падать до перезапуска клиента. Исправление: на `404`
   сбросить `session`, повторно отправить сохранённый `initialize` и повторить запрос.
   Аналогично: если при старте Edy не был запущен, мост навсегда остаётся автономным.
3. **(вероятная ошибка) `null` в конфиге Claude Code.** `claudeMCP` кладёт `x.Headers`, `x.Args`,
   `x.Env` как есть; для сервера без заголовков/аргументов/переменных это `null`
   (`climcp.go:65-67`). Схема `.mcp.json` Claude Code может отвергнуть `null` для `headers`/`env`/
   `args` — **не проверено с настоящим Claude Code**. Надёжнее не выводить пустые поля.
4. **Variadic-флаги Claude Code.** `--allowedTools` и `--disallowedTools` принимают список до
   следующего флага. Если шаблон провайдера ставит `{prompt}` сразу после `claude` (без `-p` и
   других флагов), промпт будет съеден `--disallowedTools` как имя инструмента. Все пресеты
   сейчас начинаются с `-p`, но пользовательские команды не проверяются.
5. **Нет «снимка до» у внешних клиентов.** `call` делает снимок только после правки. Если до
   первой MCP-правки в `.edy/shadow.git` не было ни одного снимка, откатить первую правку к
   исходному состоянию из интерфейса нельзя (нет коммита «до»). У задач baseline есть, у внешних
   MCP-клиентов — нет.
6. **Неизвестный инструмент считается правящим.** `writes := !readOnly[name] && !noFiles[name]`
   (`mcp.go:266`) → во время задачи на опечатку в имени клиент получит «задача Edy…», а не
   «неизвестный инструмент»; без задачи — лишний захват `mu`.
7. **`run_checks` не блокируется `Busy` и не берёт `mu`.** Внешний клиент может запустить
   `go test` параллельно с задачей и с собственными правками.
8. **Гонка `Busy`.** Проверка `Busy()` и правка не атомарны относительно старта задачи: задача
   может стартовать между ними. `mu` — свой у каждого `mcp.Server`, то есть только внутри сессии;
   две сессии одного проекта правят параллельно, и два `Shadow` делают `git add/commit` в один
   `shadow.git` одновременно (возможен `index.lock`).
9. **`edy mcp` требует Keychain** даже в автономном режиме (раздел 6) и **меняет список
   проектов**: `register` поднимает проект наверх, а список ограничен 10 (`projects.go:137-141`) —
   подключение 11-го проекта по stdio молча вытеснит самый старый из интерфейса.
10. **Точное совпадение пути проекта.** `knownProject` сравнивает строки. На macOS `/tmp` →
    `/private/tmp`, пути с `/` на конце, разный регистр (APFS нечувствительна к регистру) — всё
    даёт `404`. Мост этого избегает, беря путь из ответа `register`.
11. **`h.Addr` и `checkMCPURL`.** URL привязки строится из адреса прослушивания. Если Edy
    запущен с `-addr 0.0.0.0:11435` или на внешнем интерфейсе, `checkMCPURL` отвергнет
    `0.0.0.0` → задачи с Claude Code упадут с `400`. Надёжнее строить URL на `127.0.0.1:<порт>`.
12. **Вытеснение сессий задачи.** Сессии привязок тоже считаются в лимит 64; при большом числе
    внешних клиентов сессия работающего исполнителя может быть вытеснена → `404` посреди задачи.
    Вытесняемая сессия привязки не журналируется (у неё нет `agentID`).
13. **Тело > 8 МиБ** молча обрезается и даёт `-32700` вместо `413`. `write_file` с большим файлом
    (base64 и т.п.) упрётся в это.
14. **`serverInfo.version` всегда `"dev"`** — `Server.Version` нигде не заполняется.
15. **Нет пакетных запросов, пагинации `tools/list`, `notifications/tools/list_changed`,
    `MCP-Protocol-Version`, SSE.** Для текущих клиентов достаточно, но `resources/list` отвечает,
    хотя `resources` не объявлены в capabilities.
16. **`id: null`** считается запросом (длина сырого `null` — 4), сервер ответит с `id:null`.
17. **Редактирование сервера каталога стирает секреты.** API не отдаёт значения заголовков/env,
    а `POST` — полная замена. Если форма сохраняет сервер без повторного ввода секретов, они
    пропадут. Также `POST` всегда ставит `enabled: true` (выключенный сервер включится после
    правки).
18. **Сообщение валидации URL** говорит «должен начинаться с https://», хотя `http://` тоже
    принимается (`mcpservers.go:62-65`).
19. **`DELETE /mcp` без проекта и для несуществующей сессии** всегда `204` (без утечек, просто
    к сведению).
20. **`read_file` не отсекает двоичные файлы** (в отличие от `Workspace.ReadText`), до 200 КиБ
    бинарного мусора уйдёт клиенту.

### Что не проверено с настоящим Claude Code

Весь путь «исполнитель-Claude Code» проверен только поддельным `claude` (e2e-тест). Не проверено
на реальной утилите (на коммите `b88fcbf` нет свидетельств в репозитории):

- принимает ли текущая версия Claude Code конфиг ровно такого вида (`type:"http"`, `headers`,
  `null`-поля, раздел 13 п.3);
- что `--allowedTools mcp__edy` действительно разрешает все инструменты сервера без
  подтверждения в режиме `-p` под PTY, и что нет интерактивного запроса доверия к MCP-серверу;
- как Claude Code реагирует на `405` на `GET /mcp` (ожидается — работает без SSE-потока) и на
  `404` по сессии (должен заново делать `initialize`);
- что `--disallowedTools` с пробелами разбирается как список во всех версиях;
- переменные `env` stdio-серверов каталога: добавляются к окружению или заменяют его;
- поведение `readOnlyHint` в Cursor/Gemini CLI (аннотации необязательны для клиентов).

---

## 14. Идеи и TODO

- Исправить пункты 1–3 раздела 13 и добавить на них тесты (эпик с поддельным `claude` уже легко
  собрать из `mcp_e2e_test.go`).
- Делать снимок «до» при первой правке сессии внешнего клиента (как baseline задач) и писать
  в журнал агента id снимков, чтобы откат был в один клик.
- Общий на проект мьютекс правок/снимков (в `Handler`, по `project`), а не на `mcp.Server`.
- `Busy` проверять под тем же мьютексом, которым задача захватывает проект.
- Добавить `mcpFlags` для других CLI с поддержкой MCP из аргументов (Gemini CLI, Codex) —
  достаточно функции в `mcpCapable`.
- Заполнять `Server.Version` версией сборки.
- Поддержать `notifications/tools/list_changed` (скиллы/память появились) — нужен SSE-поток `GET`.
- В каталоге: «оставить без изменений» для секретов при редактировании; маскировать секреты в
  `url`/`args`; проверка подключения к серверу кнопкой «Проверить» (`initialize` + `tools/list`).
- Не открывать Keychain в `edy mcp`, когда база не нужна (перенести `openStore` внутрь команд).
- Нормализовать путь проекта (`EvalSymlinks`) и в `addProject`, и в `knownProject`.
- Запретить Claude Code `Read`/`Glob`/`Grep` вне проекта (или все, оставив `mcp__edy`) — сейчас
  исполнитель может читать что угодно на диске.
- Строить URL привязки на `127.0.0.1:<порт>` независимо от адреса прослушивания.
- Журналировать вытеснение сессии привязки в ленту задачи.
