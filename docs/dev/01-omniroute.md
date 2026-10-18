# 01. OmniRoute — слой доступа к моделям (прокси), веб-чаты и хранилище секретов

> Область документа: `internal/omniroute/`, `internal/webchat/`, `internal/vault/` и часть
> `internal/store/store.go`, относящаяся к провайдерам, ключам, расходам, бюджету и настройкам.
> Полная схема БД — в [06-store.md](06-store.md); MCP-сервер Edy — в [04-mcp.md](04-mcp.md);
> HTTP API интерфейса (`/api/...`) — в [05-api.md](05-api.md); как всё собирается в один процесс —
> в [08-app-build.md](08-app-build.md).
>
> Состояние описано на коммит `b88fcbf`. Все ссылки вида `файл:строка` указывают на этот коммит.
> Зависимости слоя: `github.com/anthropics/anthropic-sdk-go v1.82.0`, `github.com/chromedp/chromedp v0.16.0`,
> `github.com/creack/pty v1.1.24`, `github.com/zalando/go-keyring v0.2.8`, `modernc.org/sqlite` (через store).

---

## Содержание

1. [Зачем нужен OmniRoute](#1-зачем-нужен-omniroute)
2. [Пакеты и файлы: назначение](#2-пакеты-и-файлы-назначение)
3. [Путь запроса от начала до конца](#3-путь-запроса-от-начала-до-конца)
4. [Типы провайдеров (kind) подробно](#4-типы-провайдеров-kind-подробно)
5. [Эмуляция инструментов (toolemu)](#5-эмуляция-инструментов-toolemu)
6. [CLI-адаптер: PTY, шаблоны команд, очистка вывода, MCP, картинки](#6-cli-адаптер)
7. [Теги возможностей (caps.go)](#7-теги-возможностей-capsgo)
8. [Каталог пресетов](#8-каталог-пресетов)
9. [Веб-чаты (internal/webchat)](#9-веб-чаты-internalwebchat)
10. [Vault: шифрование секретов](#10-vault-шифрование-секретов)
11. [Хранилище: провайдеры, ключи, расходы, бюджет, настройки](#11-хранилище-провайдеры-ключи-расходы-бюджет-настройки)
12. [Справочник: все объявления по файлам](#12-справочник-все-объявления-по-файлам)
13. [Тесты](#13-тесты)
14. [Ограничения, непроверенное, грабли, идеи](#14-ограничения-непроверенное-грабли-идеи)

---

## 1. Зачем нужен OmniRoute

OmniRoute — это локальный HTTP-сервер, который делает **любой источник ИИ** похожим на OpenAI API.
Всё остальное в Edy (агент, Оркестратор, ревьюер, поиск по смыслу) говорит только на одном языке —
OpenAI `/v1/chat/completions` и `/v1/embeddings` — и выбирает модель строкой `"провайдер/модель"`,
например `anthropic/claude-opus-5-5`, `oa/gpt-x`, `agy/claude-sonnet-4-6`, `gpt/default`.

Источники бывают четырёх видов (`store.Kind*`, `internal/store/store.go:18-23`):

| kind | Что это | Адаптер |
|---|---|---|
| `openai` | Любой OpenAI-совместимый HTTP API: OpenAI, OpenRouter, DeepSeek, Groq, Google (OpenAI-режим), локальные Ollama/LM Studio/vLLM/llama.cpp | `OpenAIAdapter` (`internal/omniroute/openai.go:19`) |
| `anthropic` | Anthropic Messages API (через официальный Go SDK) | `AnthropicAdapter` (`internal/omniroute/anthropic.go:22`) |
| `cli` | Терминальная утилита (Claude Code, Antigravity, Codex, Gemini CLI, Aider, Ollama…), запускается в PTY на каждый запрос | `CLIAdapter` (`internal/omniroute/cli.go:57`) |
| `web` | Веб-чат (ChatGPT, Claude.ai, Gemini) в управляемом браузере под аккаунтом пользователя | `WebAdapter` (`internal/omniroute/web.go:19`) + `webchat.Manager` |

Ключевые функции сверху над адаптерами: авторизация по Bearer-токену, ротация API-ключей
(перерыв ключа после 429, отключение после 401/403), дневной бюджет в долларах, учёт расходов
по агентам (заголовок `X-Edy-Client`), SSE-стриминг или сборка единого JSON-ответа, проверка
наличия тегов «Картинки»/«Звук» перед отправкой медиа, эмуляция tools для текстовых провайдеров,
подключение MCP-сервера Edy к Claude Code.

**Где слушает.** Константа `DefaultAddr = "127.0.0.1:11435"` (`internal/omniroute/server.go:25`).
Сам `omniroute.Server` — это только `http.Handler`; порт слушает `internal/app/app.go`, который
монтирует его на `/v1/` и `/healthz` в общий `ServeMux` вместе с `/api/` (API интерфейса),
`/mcp` (MCP-сервер Edy) и `/` (UI) (`internal/app/app.go:166-171`). То есть на одном порту 11435 живут
и прокси, и интерфейс, и MCP — с **одним и тем же токеном**.

**Внутренние клиенты** (агент, Оркестратор, ревьюер, эмбеддинги) обращаются к роутеру не по сети,
а через `agent.InProcess(router)` (`internal/agent/llm.go:170`) — `http.Client`, транспорт которого
вызывает `ServeHTTP` напрямую через `httptest.ResponseRecorder`. Базовый URL у них фиктивный —
`http://edy.local/v1` (`internal/app/app.go:113-118`). Следствие: для внутренних клиентов
стриминг фактически буферизуется целиком (см. раздел 14).

**Токен.** Генерируется `NewToken()` (`server.go:77`) в формате `edy-<48 hex>`; хранится в таблице
`settings` под ключом `router_token` и копируется в файл `<home>/router.token` с правами `0600`
(`internal/app/app.go:31,62-75`). Внешние программы (например, Cursor/Continue или сам `edy mcp`)
читают его из файла.

---

## 2. Пакеты и файлы: назначение

### 2.1. `internal/omniroute`

- **`server.go`** — сердце пакета. Тип `Server` (HTTP-обработчик), маршрутизация (`/healthz`,
  `GET /v1/models`, `POST /v1/chat/completions`, `POST /v1/embeddings`), проверка Bearer-токена,
  разбор модели `провайдер/модель`, выбор адаптера по `provider.Kind`, проверка медиа по тегам,
  обёртка `toolEmuAdapter` для CLI/web, проверка дневного бюджета, разбор MCP-заголовков,
  цикл ротации ключей `callWithRotation` с перерывами (cooldown) и отключением ключей, запись
  расхода в `usage`, а также `responder` — запись ответа клиенту SSE-потоком или одним JSON.
- **`adapter.go`** — контракт адаптера: `Adapter` (один метод `Stream`), `Call` (провайдер, ключ,
  модель, запрос, рабочая папка, MCP), `Event` (дельта + `FinishReason`), `UpstreamError`
  (HTTP-ошибка провайдера до начала генерации, по ней роутер решает о ротации) и `retryAfter`
  (разбор заголовка `Retry-After` в секундах или HTTP-дате).
- **`types.go`** — структуры формата OpenAI: `ChatRequest` (с `Raw` — исходным телом, чтобы
  пробрасывать OpenAI-провайдерам все поля без потерь), `Message`, `ContentPart`, `ImageURL`,
  `InputAudio`, `Tool`, `ToolCall`, `Usage`, `Delta`, `Chunk`, `Completion`; `parseChatRequest`
  и `splitModel` (разрезает по **первому** `/`).
- **`openai.go`** — `OpenAIAdapter`: копирует исходное тело, заменяет `model`, принудительно
  включает `stream: true` и `stream_options.include_usage`, шлёт на `<base>/chat/completions`
  и переводит SSE провайдера в `Event` как есть (только `choices[0]`).
- **`anthropic.go`** — `AnthropicAdapter`: перевод OpenAI-запроса в Anthropic Messages API
  (system/developer → `system`, tool_calls → `tool_use`, tool → `tool_result`, картинки → image
  blocks, tools → `ToolParam` с `input_schema`), стриминг через SDK и обратный перевод событий в
  OpenAI-дельты (текст, tool_calls с индексами, finish_reason, usage).
- **`cli.go`** — `CLIAdapter`: запуск утилиты в PTY, подстановка `{model}`/`{prompt}`, поиск
  исполняемого файла (`FindExecutable`, в том числе в `~/.local/bin`, `/opt/homebrew/bin`,
  `/usr/local/bin`), сворачивание диалога в один промпт (`renderPrompt`), оценка токенов,
  убийство группы процессов при отмене, диагностика «последнего вывода».
- **`climcp.go`** — подключение MCP-сервера Edy (и внешних MCP-серверов из каталога) к CLI-утилите.
  Сейчас умеет только Claude Code (`claude`): временный JSON-конфиг `--mcp-config`,
  `--strict-mcp-config`, `--allowedTools`, `--disallowedTools`. Заголовки `X-Edy-MCP`,
  `X-Edy-MCP-Servers`, проверка, что MCP-адрес локальный.
- **`cliimages.go`** — превращение картинок из `data:`-URL в временные файлы для CLI-утилит
  (утилита получает путь текстом и открывает файл сама); для `claude` добавляется `--add-dir`.
- **`sanitize.go`** — `TermSanitizer`: превращает сырой поток терминала в чистый текст (удаляет
  ANSI CSI/OSC, обрабатывает `\r`, `\b`, кадры спиннеров-Брайля, разорванные между чтениями
  ESC-последовательности и UTF-8-символы).
- **`toolemu.go`** — `toolEmuAdapter`: эмуляция function calling для CLI и веб-чатов через
  протокол `<tool_call>{json}</tool_call>` в тексте.
- **`caps.go`** — теги возможностей провайдеров (`mcp`, `tools`, `vision`, `audio`, `voice`,
  `image_gen`, `web`, `long_context`), каталог для UI, умолчания по типу/утилите, `Caps`/`HasCap`.
- **`presets.go`** — каталоги готовых профилей: `CLIPresets` (13 терминальных агентов с командой
  запуска и командой установки) и `APIPresets` (20 API-провайдеров с базовым URL и ссылкой на ключи).
- **`embeddings.go`** — обработчик `/v1/embeddings`: только `openai`-провайдеры, своя (упрощённая)
  ротация ключей, бюджет и учёт расходов; ответ провайдера проксируется байт-в-байт.
- **`models.go`** — `ListModels`: загрузка списка моделей у провайдера (`GET <base>/models` для
  OpenAI-совместимых, `Models.ListAutoPaging` для Anthropic). Используется API интерфейса
  (кнопка «Загрузить модели»), а не самим прокси.
- **`web.go`** — `WebAdapter` и интерфейс `WebAsker`: рендерит диалог в промпт, отдаёт его
  `webchat.Manager.Ask` и превращает «полный текст ответа на странице» в поток дельт.
- Тесты: `server_test.go`, `cli_test.go`, `climcp_test.go`, `toolemu_test.go`, `sanitize_test.go`,
  `models_test.go` (см. раздел 13).

### 2.2. `internal/webchat`

- **`manager.go`** — `Manager`: отдельный браузер (процесс + папка профиля) на каждое
  веб-подключение, окно входа без автоматизации, проверка входа, отправка сообщения (вставка текста,
  Enter, запасная кнопка), опрос страницы до окончания ответа, вежливость (один запрос за раз,
  пауза, дневной лимит), статусы для UI, диагностика при сбое (скриншот + JSON-дамп полей и кнопок
  в `web-debug/`). Все JS-скрипты, исполняемые на странице, — константы в этом файле.
- **`chrome.go`** — документация пакета (принципы «вести себя как человек, не маскироваться»),
  поиск браузера (`FindBrowser`: скачанный Chromium или установленный Google Chrome, переменная
  `EDY_CHROME`), скачивание snapshot-сборки Chromium и безопасная распаковка zip с симлинками.
- **`sites.go`** — `Site` (CSS-селекторы и параметры вежливости), встроенные `DefaultSites`
  (chatgpt, claude, gemini), `LoadSites` с переопределением из `<home>/web-sites.json`.
- **`webchat_test.go`** — интеграционные тесты с настоящим headless Chrome и фейковыми страницами
  чата (пропускаются, если Chrome не найден).

### 2.3. `internal/vault`

- **`vault.go`** — AES-256-GCM поверх 32-байтного мастер-ключа; мастер-ключ хранится в системной
  связке ключей (macOS Keychain, сервис `dev.edy.ide`, аккаунт `master-key`) и создаётся при
  первом запуске. Формат шифротекста: `nonce(12) || ciphertext+tag`.
- **`vault_test.go`** — круговое шифрование, отказ при подмене, стабильность ключа из
  (мок-)Keychain.

### 2.4. `internal/store/store.go` (часть, относящаяся к OmniRoute)

Таблицы `providers`, `api_keys`, `settings`, `usage` (`store.go:30-62`) и миграции колонок
`command`, `site`, `browser`, `caps` (`store.go:65-72`), тип `Provider` с методом `Cost`, `Key`,
`KeyInfo`, `Usage`, `UsageSummary` и CRUD: `AddProvider`, `SetModels`, `SetCaps`, `DeleteProvider`,
`Provider`, `Providers`, `AddKey`, `Keys`, `KeyInfos`, `DeleteKey`, `DisableKey`, `Setting`,
`SetSetting`, `RecordUsage`, `UsageSince`, `SpentSince`. Подробно — раздел 11.

---

## 3. Путь запроса от начала до конца

### 3.1. Схема

```
 Клиент (агент Edy через InProcess, или внешняя программа по сети на 127.0.0.1:11435)
   │  POST /v1/chat/completions
   │  Authorization: Bearer edy-…           ← обязательно
   │  X-Edy-Client: agent|orchestrator|reviewer|…   ← для учёта расходов (необязательно)
   │  X-Edy-Workdir: /путь/к/проекту         ← рабочая папка CLI (необязательно)
   │  X-Edy-MCP: http://127.0.0.1:11435/mcp?…  ← подключить MCP Edy к CLI (необязательно)
   │  X-Edy-MCP-Servers: github,linear       ← внешние MCP из каталога (только вместе с X-Edy-MCP)
   ▼
 Server.ServeHTTP (server.go:85)
   ├─ /healthz → "ok" (без токена)
   ├─ authorized()? нет → 401 authentication_error
   ├─ GET  /v1/models          → handleModels (server.go:111)
   ├─ POST /v1/embeddings      → handleEmbeddings (embeddings.go:18)
   ├─ POST /v1/chat/completions→ handleChat (server.go:131)
   └─ иначе → 404 not_found
           │
           ▼
 handleChat
   1. тело ≤ 32 МиБ → parseChatRequest (types.go:25): model и messages обязательны → 400
   2. splitModel("prov/model/с/слешами") → 400, если нет "/"
   3. store.Provider(prov) → 404, если нет; 500 при ошибке БД
   4. adapters[provider.Kind] → 500, если адаптера нет (например, web без RegisterAdapter)
   5. mediaIn(messages): картинки без CapVision / звук без CapAudio → 400
   6. tools есть и kind ∈ {cli, web} → adapter = toolEmuAdapter{inner: adapter}
   7. checkBudget(): потрачено с полуночи ≥ daily_budget_usd → 429 budget_exceeded
   8. Call{Provider, Model, Request, Workdir: X-Edy-Workdir}
   9. X-Edy-MCP: checkMCPURL (только http://localhost|127.0.0.1|::1 …/mcp) → 400;
      провайдер не cli или утилита не умеет MCP → 400;
      X-Edy-MCP-Servers → extraMCP (включённые серверы каталога) → 400, если имени нет;
      call.MCP = {URL, Token: токен OmniRoute, Extra}
  10. responder := newResponder(w, stream, "prov/model")
  11. callWithRotation(ctx, adapter, call, responder.emit)
           │
           ▼
 callWithRotation (server.go:253)
   keys := store.Keys(provider.ID)   (только disabled=0, расшифрованные)
   нет ключей → один «пустой» ключ {ID:-1}  (локальные провайдеры, CLI, web)
   for key in keys:
       если key на перерыве (cooldowns[key.ID] > now) → пропустить
       call.Key = key.Secret
       usage, err = adapter.Stream(ctx, call, trackedEmit)
       если err == nil или уже что-то ушло клиенту или err не UpstreamError → вернуть
       429            → coolDown(key, Retry-After или 1 мин), следующий ключ
       401/403 (ID>0) → store.DisableKey(key) навсегда, следующий ключ
       иначе          → вернуть ошибку
   все ключи на перерыве → UpstreamError 429 «все ключи … на перерыве»
           │
           ▼
 Адаптер по kind
   openai    → POST <base>/chat/completions (stream) → SSE → Event
   anthropic → SDK Messages.NewStreaming → события → Event
   cli       → PTY-процесс → TermSanitizer → Event (построчно)
   web       → webchat.Manager.Ask → опрос DOM → Event (приращения текста)
   (toolEmu) → буферизует весь текст inner-адаптера → <tool_call> → tool_calls
           │
           ▼
 handleChat (после возврата)
   usage.Prompt+Completion > 0 → store.RecordUsage{provider, model, client, in, out, cost}
   err → responder.fail(err): до первого байта — HTTP-ошибка со статусом UpstreamError
         (или 502), после начала SSE — событие data: {"error":…}
   ok  → responder.finish(usage): SSE — финальный chunk с usage + "data: [DONE]";
         без stream — один JSON chat.completion
```

### 3.2. Маршруты

| Метод и путь | Обработчик | Токен | Что делает |
|---|---|---|---|
| `GET /healthz` | `ServeHTTP` (`server.go:86-89`) | не нужен | Отвечает `ok`. Используется `app.Running` для проверки «уже запущен ли Edy». |
| `GET /v1/models` | `handleModels` (`server.go:111`) | нужен | Список `провайдер/модель` для всех провайдеров из БД (по полю `Models`, без запроса к провайдерам). Формат OpenAI: `{"object":"list","data":[{"id","object":"model","owned_by"}]}`. |
| `POST /v1/chat/completions` | `handleChat` (`server.go:131`) | нужен | Основной маршрут, см. схему. |
| `POST /v1/embeddings` | `handleEmbeddings` (`embeddings.go:18`) | нужен | Эмбеддинги, только `openai`-провайдеры. |
| всё остальное | — | нужен | `404 not_found` «неизвестный маршрут». |

Обратите внимание: проверка токена идёт **до** маршрутизации, поэтому неизвестный маршрут без
токена даёт 401, а не 404. Метод `HEAD`/`OPTIONS` не поддержан (CORS нет — к прокси ходят не из браузера).

### 3.3. Авторизация

`authorized` (`server.go:106`) берёт заголовок `Authorization`, требует префикс ровно `Bearer `
(с заглавной B и одним пробелом) и сравнивает остаток с токеном через
`subtle.ConstantTimeCompare`. Ошибка — `401 {"error":{"type":"authentication_error",...}}`.

### 3.4. Разбор модели и провайдера

- `splitModel` (`types.go:44`) режет по **первому** `/`: `openrouter/meta-llama/llama-3-70b` →
  провайдер `openrouter`, модель `meta-llama/llama-3-70b`. Пустой провайдер или модель — ошибка.
- Имя провайдера — это `providers.name` (уникально), задаётся пользователем при добавлении
  (`edy provider add` или UI). Модель после `/` **не проверяется** по списку `provider.Models` —
  можно звать любую модель, даже не загруженную в список.
- В ответ клиенту в поле `model` возвращается исходная строка `провайдер/модель`
  (`newResponder(..., req.Model, ...)`, `server.go:201`).

### 3.5. Проверка медиа

`mediaIn` (`server.go:453`) ищет в сообщениях части `image_url` и `input_audio`. Если найдены
картинки, а у провайдера нет тега `vision` — 400 «модель … не видит картинки — включите ей тег
«Картинки»»; аналогично для звука и тега `audio` (`server.go:162-171`). Теги — на уровне
**провайдера**, а не модели (см. раздел 7).

### 3.6. Дневной бюджет

`checkBudget` (`server.go:227`): читает настройку `daily_budget_usd` (`SettingDailyBudget`,
`server.go:28`). Нет настройки, не число или `<= 0` — лимита нет. Иначе считает
`store.SpentSince(полночь по локальному времени)` и, если `spent >= limit`, отдаёт
`429 {"error":{"type":"budget_exceeded","message":"дневной бюджет исчерпан: потрачено $X из $Y"}}`.
Проверка — **до** запроса; запрос, начатый при остатке $0.01, может уйти сильно за лимит.
Задаётся из CLI `edy budget <usd>` (`cmd/edy/main.go:113`) или через API интерфейса
(`internal/api/api.go:696-710`).

### 3.7. Ротация ключей, перерывы, отключение

`callWithRotation` (`server.go:253-299`):

- Ключи берутся `store.Keys(providerID)` — **только включённые** (`disabled = 0`), в порядке
  добавления (`ORDER BY id`), уже расшифрованные vault'ом. Ошибка расшифровки любого ключа —
  ошибка всего запроса.
- Нет ключей → используется фиктивный ключ `{ID: -1, Secret: ""}`: адаптеры не ставят
  `Authorization`, CLI и web ключи вообще не используют.
- Перерывы (`cooldowns map[int64]time.Time`, `server.go:47`) хранятся **только в памяти**
  процесса под мьютексом; после перезапуска Edy все ключи снова «свежие». Длительность —
  `Retry-After` (секунды или HTTP-дата, `adapter.go:48`) или `defaultCooldown = 1 мин`
  (`server.go:34`).
- Переключение на другой ключ возможно, **только пока клиенту не ушёл ни один `Event`**
  (`emitted`, `server.go:264-268`). Это принципиально: нельзя склеить ответ из двух генераций.
  Ошибка в середине потока уходит клиенту как есть (см. `responder.fail`).
- Решение о ротации принимается только по `*UpstreamError` (через `errors.As`):
  - `429` → ключ на перерыв, лог `Info`, следующий ключ;
  - `401`/`403` и `ID > 0` → `store.DisableKey` (флаг `disabled=1` навсегда; включить обратно
    можно только удалив и добавив ключ заново), лог `Warn`, следующий ключ;
  - любой другой статус → ошибка возвращается сразу.
- Если цикл закончился без единой попытки (все на перерыве) — `UpstreamError{429, "все ключи
  провайдера X временно на перерыве после лимита"}`. Если попытки были — последняя ошибка.

### 3.8. Учёт расходов

После вызова адаптера (`server.go:204-216`), **даже при ошибке**, если
`PromptTokens + CompletionTokens > 0`, пишется строка в `usage`:
`provider`, `model` (без префикса), `client` (= заголовок `X-Edy-Client`, может быть пустым),
`input_tokens`, `output_tokens`, `cost_usd = provider.Cost(in, out)` где
`Cost = (in*PriceIn + out*PriceOut) / 1e6` (`store.go:95`) — цены в долларах за миллион токенов,
задаются у провайдера. Ошибка записи только логируется.

Источник токенов по типам:
- `openai` — из последнего SSE-чанка с `usage` (просим его через `stream_options.include_usage`);
  если провайдер не прислал usage — **расход не записывается вообще**;
- `anthropic` — `message_start.usage.input_tokens + cache_read + cache_creation` и
  `message_delta.usage.output_tokens`;
- `cli` и `web` — оценка `estimateTokens` ≈ `(руны + 3) / 4` (`cli.go:219`) по промпту и выводу.

Известные клиенты (`X-Edy-Client`): `orchestrator`, `agent`, `reviewer`, `embeddings`
(`internal/app/app.go:113-118`); внешние программы обычно его не ставят — их расход идёт с пустым
`client`. Сводка — `store.UsageSince` (`store.go:357`).

### 3.9. Ответ клиенту: `responder`

`responder` (`server.go:318-440`) пишет ответ в одном из двух режимов в зависимости от `stream`
в запросе (адаптеры **всегда** работают потоком, режим влияет только на запись):

**SSE (`stream: true`)** — `emit` (`server.go:337`):
- при первом событии ставит `Content-Type: text/event-stream`, `Cache-Control: no-cache`,
  `Connection: keep-alive`, `200 OK`;
- каждое событие — `data: {"id":"chatcmpl-…","object":"chat.completion.chunk","created",…,
  "model":"prov/model","choices":[{"index":0,"delta":{…},"finish_reason":null|"stop"|…}]}\n\n`
  с `Flush()`;
- `finish` (`server.go:397`): если ничего не было отправлено — сначала шлёт пустую дельту
  `role: assistant`; затем чанк с `choices: []` и `usage` (как `include_usage` у OpenAI), затем
  `data: [DONE]`.

**Обычный JSON** — `emit` копит `content` и склеивает фрагменты `tool_calls` по `index`
(`mergeToolCalls`, `server.go:377`); `finish` пишет `chat.completion` с одним `choice`. Поле
`content` — `null`, если есть tool_calls и текста нет; `finish_reason` по умолчанию `stop`;
`index` у tool_calls в итоговом ответе обнуляется (в формате OpenAI его там нет).

**Ошибки** — `fail` (`server.go:424`): статус берётся из `UpstreamError.Status` (иначе `502`),
тип — `rate_limit_error` для 429, иначе `upstream_error`. Если SSE уже начат, пишется событие
`data: {"error":{"type","message"}}` **без** `[DONE]`.

Формат всех ошибок: `{"error":{"type":"…","message":"…"}}` (`writeError`, `server.go:448`).
Таблица кодов:

| Код | type | Когда |
|---|---|---|
| 400 | `invalid_request_error` | битый JSON, нет model/messages, model без `/`, медиа без тега, плохой `X-Edy-MCP`, MCP у неподходящего провайдера, неизвестный внешний MCP; также ошибки адаптеров `UpstreamError{400}` (перевод запроса, tools у адаптера без эмуляции, только текст у CLI и т.п.) |
| 401 | `authentication_error` | нет/неверный токен OmniRoute |
| 401/403 | `upstream_error` | **ключ провайдера** отклонён и других ключей нет (внимание: клиент видит тот же код, что и при неверном токене OmniRoute) |
| 404 | `invalid_request_error` / `not_found` | провайдер не настроен / неизвестный маршрут |
| 429 | `budget_exceeded` | дневной бюджет |
| 429 | `rate_limit_error` | все ключи на перерыве / провайдер вернул 429 последнему ключу |
| 500 | `server_error` | ошибка БД, нет адаптера для kind |
| 502 | `upstream_error` | сетевые ошибки, CLI упала, веб-чат не ответил, неожиданный SSE |

---

## 4. Типы провайдеров (kind) подробно

### 4.1. `openai` — `OpenAIAdapter` (`openai.go`)

**Настройка.** `Provider.Kind = "openai"`, `BaseURL` (пусто → `https://api.openai.com/v1`,
`defaultOpenAIBaseURL`, `openai.go:14`), ключи (необязательно — локальным серверам не нужны),
`Models`, `PriceIn`/`PriceOut`. Удобнее всего добавлять через пресеты `APIPresets` (раздел 8).

**Перевод запроса** (`openai.go:23-34`). Берётся **исходное тело** `ChatRequest.Raw` (все поля
клиента: `temperature`, `response_format`, `tools`, `tool_choice`, `seed`… сохраняются), затем
перезаписываются: `model` = имя без префикса, `stream = true`,
`stream_options = {"include_usage": true}`. Запрос: `POST <base>/chat/completions`,
`Accept: text/event-stream`, `Authorization: Bearer <ключ>` (если ключ есть).

**Стриминг.** Ответ не-2xx → `UpstreamError{Status, RetryAfter, Message: первые 4 КиБ тела}`
— по нему работает ротация. 2xx → построчное чтение (`bufio.Scanner` с буфером до 8 МиБ на
строку), берутся только строки `data:`; `[DONE]` завершает; каждый `Chunk` разбирается как
OpenAI-чанк; `usage` запоминается; из `choices` берётся только `index == 0` (n>1 не поддержан);
`delta` пробрасывается как есть, `finish_reason` — в `Event.FinishReason`.

**Tool calls.** Нативные: `tools` уходят провайдеру в исходном виде, дельты `tool_calls`
приходят обратно без изменений. Эмуляции для `openai` нет — даже если у провайдера снят тег
`tools`, `tools` всё равно уйдут провайдеру (тег на это не влияет, см. раздел 7).

**Картинки и звук.** Пробрасываются как есть (`image_url`, `input_audio`) — если у провайдера
включены теги `vision`/`audio`. По умолчанию у `openai`-провайдера есть только `tools`, поэтому
для картинок тег надо включить вручную.

**Ограничения.** Нет таймаута HTTP-клиента (используется `http.DefaultClient`, ограничение
только контекстом запроса). Ошибки, пришедшие **внутри** SSE (`data: {"error":…}`), разбираются
как пустой чанк и молча теряются. Провайдеры, не понимающие `stream_options`, могут ответить
400. Провайдеры, не присылающие `usage` в стриме, не попадают в учёт расходов и бюджет.

### 4.2. `anthropic` — `AnthropicAdapter` (`anthropic.go`)

**Настройка.** `Kind = "anthropic"`, `BaseURL` необязателен (по умолчанию SDK ходит на
`api.anthropic.com`), ключи `sk-ant-…` (передаются в `X-Api-Key` через `option.WithAPIKey`).
SDK создаётся на каждый вызов с `option.WithMaxRetries(0)` — повторами управляет OmniRoute.

**Перевод запроса** — `toAnthropicParams` (`anthropic.go:127`):
- `MaxTokens` = `max_completion_tokens` → `max_tokens` → `defaultAnthropicMaxTokens = 32000`
  (`anthropic.go:17`). В Anthropic поле обязательно.
- `stop` (строка или массив) → `StopSequences`.
- **Sampling-параметры (`temperature`, `top_p`) намеренно не передаются** — новые модели Claude
  отвечают на них 400 (комментарий `anthropic.go:19-21`, проверяется тестом). Также не
  передаются `response_format`, `tool_choice`, `seed`, `user`, `metadata` и пр.
- `tools`: поддержан только `type: "function"` (или пустой) — иначе 400. `parameters` →
  `ToolInputSchemaParam` (`toInputSchema`, `anthropic.go:270`): `properties` и `required`
  отдельно, остальные ключи (например, `additionalProperties`) — в `ExtraFields`, `type`
  выбрасывается (SDK сам ставит `object`).
- Сообщения:
  - `system`/`developer` → в `params.System` (все такие сообщения поднимаются наверх, где бы ни
    стояли);
  - `user` → блоки `text` и `image` (`userBlocks`, `anthropic.go:226`); `input_audio` → ошибка
    «Anthropic API не принимает звук»;
  - `assistant` → `text` + `tool_use` (аргументы, не являющиеся валидным JSON, заменяются на `{}`);
  - `tool` → `tool_result` в **user**-сообщении;
  - соседние сообщения одной роли сливаются (`add`, `anthropic.go:165`) — Anthropic ожидает,
    что результаты нескольких инструментов придут одним сообщением;
  - неизвестная роль → ошибка; нет ни одного не-system сообщения → «нет сообщений пользователя».
- Картинки (`imageBlock`, `anthropic.go:257`): `data:<mime>;base64,<data>` → base64-источник;
  любой другой URL → URL-источник (Anthropic скачает сам). `data:` без `;base64` — ошибка.
- Ошибка перевода → `UpstreamError{400}` (без ротации).

**Стриминг** (`anthropic.go:44-107`): на первом событии отдаётся `role: assistant`. Далее:

| Событие Anthropic | Что уходит клиенту |
|---|---|
| `message_start` | ничего; запоминается `PromptTokens = input + cache_read + cache_creation` |
| `content_block_start` с `tool_use` | `tool_calls[{index: N, id, type: function, function.name}]`, N — порядковый номер tool_use-блока (а не индекс блока Anthropic) |
| `content_block_delta` `text_delta` | `content` |
| `content_block_delta` `input_json_delta` | `tool_calls[{index: N, function.arguments: partial}]` |
| `content_block_delta` прочие (thinking, signature, citations) | ничего |
| `message_delta` | `finish_reason` (`finishReason`, `anthropic.go:110`), `CompletionTokens = output_tokens` |
| остальные | ничего |

Маппинг `stop_reason`: `end_turn`/`stop_sequence`/`pause_turn` → `stop`; `max_tokens`/
`model_context_window_exceeded` → `length`; `tool_use` → `tool_calls`; `refusal` →
`content_filter`; неизвестное → `stop`.

**Ошибки.** Если поток завершился ошибкой SDK **до первого события** и это `*anthropic.Error` —
`UpstreamError{StatusCode, RetryAfter из заголовков}` → работает ротация (тест
`TestAnthropicRotationOn429`). После начала — обычная ошибка.

**Ограничения.** Нет prompt caching (не ставятся `cache_control`), нет extended thinking, нет
документов/PDF, нет картинок в `tool_result` (берётся только текст), стоимость кэша считается по
обычной цене входа.

### 4.3. `cli` — `CLIAdapter` (`cli.go`)

Подробно — в [разделе 6](#6-cli-адаптер). Кратко: на каждый запрос запускается новый процесс
утилиты в PTY 500×50 с командой из `Provider.Command`, в которую подставлены `{model}` и
`{prompt}`; весь диалог сворачивается в один текстовый промпт; вывод очищается от терминального
мусора и отдаётся построчно; токены оцениваются. Tools — только через эмуляцию; картинки —
через временные файлы; звук — нет; MCP — только для `claude`.

### 4.4. `web` — `WebAdapter` (`web.go`) + `webchat.Manager`

**Настройка.** `Kind = "web"`, `Site` — ключ сайта (`chatgpt`, `claude`, `gemini` или свой из
`web-sites.json`), `Browser` — `chromium` (изолированный, скачивается) или `chrome`
(установленный Google Chrome с отдельной папкой профиля). Ключи не нужны. Адаптер **не**
создаётся в `NewServer` — его регистрирует сборка процесса:
`router.RegisterAdapter(store.KindWeb, &omniroute.WebAdapter{Browser: web})`
(`internal/app/app.go:110-111`, `cmd/edy/main.go:324-325`). Без этого запрос к web-провайдеру
даст 500 «нет адаптера для типа "web"».

**Поток** (`web.go:23-62`):
1. Нет `Site` → 500; есть `tools` → 400 (на практике недостижимо: сервер оборачивает web в
   `toolEmuAdapter`, который убирает tools из внутреннего запроса).
2. `renderPrompt(messages)` — тот же, что у CLI (только текст; иначе 400).
3. Сразу отдаётся `role: assistant`.
4. `Browser.Ask(ctx, Conn{Profile: provider.Name, Site, Browser}, prompt, onText)`. Профиль
   браузера = **имя провайдера** — у каждого подключения своя папка cookies.
5. `onText(full)` получает **полный** текущий текст ответа на странице; адаптер отдаёт только
   приращение `full[len(sent):]`. Если сайт переписал начало ответа (перерисовал markdown),
   приращения перестают отдаваться (см. грабли).
6. Токены оцениваются по промпту и отправленному тексту. Ошибка Ask → `UpstreamError{502}`
   (кроме `context.Canceled`).
7. В конце `finish_reason: stop`.

**Ограничения.** Только текст (картинки не загружаются, хотя теоретически сайты их понимают);
tools — только эмуляция; каждый запрос открывает **новый чат** (`Navigate(s.URL)`), поэтому
вся история передаётся в каждом промпте; usage — оценка. Подробно о браузере — раздел 9.

### 4.5. Как добавить новый kind

1. Константа в `internal/store/store.go:18-23`.
2. Тип, реализующий `Adapter.Stream` (контракт: отдавать `Event` по мере генерации, ошибки до
   начала генерации возвращать как `*UpstreamError` с HTTP-статусом — тогда заработают ротация
   и правильные коды; вернуть `Usage`).
3. Регистрация в `NewServer` (`server.go:58-62`) или через `RegisterAdapter`.
4. Решить: нужна ли эмуляция tools (условие `server.go:173`), умолчания тегов
   (`DefaultCaps`, `caps.go:54`), поддержка `ListModels` (`models.go:24`), эмбеддингов
   (`embeddings.go:44`), валидация в API добавления провайдера (`internal/api/api.go`) и CLI
   (`cmd/edy/main.go`).

---

## 5. Эмуляция инструментов (toolemu)

**Когда включается.** В `handleChat`: `len(req.Tools) > 0 && kind ∈ {cli, web}`
(`server.go:173-175`). Теги провайдера на это не влияют. Для `openai`/`anthropic` эмуляции нет.

**Переписывание запроса** — `withEmulatedTools` (`toolemu.go:68`):
1. Создаётся новый `ChatRequest` **без** `Tools` и без `Raw` (сохраняются `Model`, `Stream`,
   `MaxTokens`, `MaxCompletionTokens`).
2. Первым сообщением вставляется `system` с текстом `toolProtocol` (`toolemu.go:21-34`) и списком
   инструментов в виде:
   ```
   - <name>: <description>
     Параметры (JSON Schema): <parameters или {}>
   ```
   Текст протокола (дословно):
   ```
   Ты работаешь как агент и можешь вызывать инструменты.

   Чтобы вызвать инструмент, напиши в ответе блок ровно в таком виде:
   <tool_call>{"name": "имя_инструмента", "arguments": {...}}</tool_call>

   Правила:
   - Внутри блока — только JSON-объект с полями "name" и "arguments".
   - Можно написать несколько блоков подряд, они выполнятся по порядку.
   - После блоков ничего не пиши: дождись результатов, они придут следующим сообщением.
   - Сам ничего не выполняй и не выдумывай результаты инструментов.
   - Если инструменты больше не нужны, ответь обычным текстом без блоков.

   Доступные инструменты:
   ```
3. История: сообщения `assistant` с `tool_calls` превращаются в текст + блоки
   `<tool_call>{"name": "...", "arguments": ...}</tool_call>` (невалидные аргументы → `{}`);
   сообщения `tool` → `role: tool` с текстом `"[<tool_call_id>]\n<результат>"`; остальные —
   без изменений (включая исходные `system`, которые окажутся после протокольного).
4. Дальше внутренний адаптер (`CLIAdapter`/`WebAdapter`) сворачивает всё в один промпт через
   `renderPrompt` — заголовки `### Инструкции`, `### Пользователь`, `### Ассистент`,
   `### Результат инструмента`.

**Разбор ответа** — `parseToolCalls` (`toolemu.go:115`):
- Регулярка `(?s)<tool_call>\s*(.*?)\s*</tool_call>` (`toolemu.go:110`), нежадная.
- С тела снимаются обёртки ```` ```json ```` / ```` ``` ````.
- JSON `{"name", "arguments"}`; если JSON неверный **или имя не из списка `tools` запроса** —
  блок остаётся в тексте как есть (агент увидит его и сможет исправиться).
- Пустые/`null` аргументы → `{}`. Каждому вызову — новый id `call_<16 hex>` (`newCallID`).
- Оставшийся текст (без распознанных блоков) обрезается по краям и становится `content`.

**Что получает клиент** (`toolemu.go:36-64`): адаптер-обёртка **глотает все события** внутреннего
адаптера и копит текст; после завершения отдаёт одним событием `role: assistant` + `content`,
затем (если есть вызовы) одно событие `tool_calls` с проставленными `index` 0..N-1 и
`finish_reason: tool_calls`; иначе `finish_reason: stop`. То есть **при эмуляции стрима нет** —
клиент получает весь ответ разом в конце, даже если просил `stream: true`.

**Тонкости.**
- `emitted` в `callWithRotation` остаётся `false` всё время работы внутреннего адаптера, так что
  ротация теоретически возможна и после частичной генерации (для CLI/web ключей нет, поэтому на
  практике неважно).
- Названия инструментов в результатах не передаются — только id вызова; модель должна сама
  сопоставить `[call_…]` со своим вызовом.
- Блок внутри примера кода или незакрытый блок (модель оборвалась) обрабатываются «как есть»:
  первый будет исполнен, второй останется текстом.
- Если аргументы содержат строку `</tool_call>`, нежадная регулярка обрежет JSON — блок
  останется текстом.

---

## 6. CLI-адаптер

### 6.1. Поток `CLIAdapter.Stream` (`cli.go:59-162`)

1. `tools` в запросе → 400 (на практике сюда приходят только запросы без tools: с tools сервер
   оборачивает адаптер в эмуляцию).
2. `materializeImages` (`cliimages.go:22`) — картинки в файлы (см. 6.6); звук → 400.
3. `renderPrompt` (`cli.go:189`): если в диалоге **одно** `user`-сообщение — его текст как есть;
   иначе диалог сворачивается в текст:
   ```
   ### Инструкции
   <system>

   ### Пользователь
   <user>

   ### Ассистент
   <assistant>

   ### Результат инструмента
   <tool>

   ### Ассистент
   ```
   Любая нетекстовая часть → ошибка «CLI-провайдеры принимают только текст» (картинки к этому
   моменту уже заменены текстом).
4. `expandCommand(Provider.Command, model, prompt)` (`cli.go:171`) — см. 6.2.
5. `FindExecutable(args[0])` (`cli.go:31`): сначала `exec.LookPath`, затем, если имя без `/`,
   — `~/.local/bin`, `/opt/homebrew/bin`, `/usr/local/bin` (`extraBinDirs`, `cli.go:21`): у
   приложения, запущенного из Finder/Dock, `PATH` минимальный. Не найдено → 502 «установите её
   или добавьте провайдера заново через edy provider add».
6. Для `claude` при наличии картинок: `--add-dir <папка картинок>` сразу после имени программы
   (`cli.go:81-84`).
7. Если `call.MCP != nil` — флаги MCP из `mcpCapable[basename]` (см. 6.5), тоже сразу после имени
   программы, **перед** `--add-dir`.
8. `exec.CommandContext(ctx, bin, args[1:]...)`, `Dir = X-Edy-Workdir` (пусто — текущая папка
   процесса Edy), окружение = окружение Edy + `TERM=xterm-256color`, `NO_COLOR=1`, `CLICOLOR=0`.
9. `cmd.Cancel` убивает **всю группу процессов** (`kill(-pid, SIGKILL)`): `pty.Start` делает
   процесс лидером новой сессии, а утилиты вроде aider порождают детей.
10. `pty.StartWithSize(cmd, 500 колонок × 50 строк)` — широкий терминал, чтобы меньше переносов.
11. Сразу отдаётся `role: assistant`. Чтение PTY кусками по 4 КиБ → `TermSanitizer.Write` →
    готовые (завершённые `\n`) строки уходят клиенту. Если `emit` вернул ошибку (клиент ушёл) —
    процесс убивается, дочитывание продолжается до EOF.
12. Конец чтения: `EOF` (macOS), `EIO` (Linux) или `ErrClosed` — нормальное завершение.
13. `Pending()` — незавершённая строка (часто вопрос утилиты «Trust this folder? (y/n)») —
    запоминается для диагностики; при нормальном завершении остаток `Flush()` тоже отдаётся.
14. `cmd.Wait()`; usage — оценка.
15. Ошибки:
    - отмена контекста → `"<утилита> остановлена (context …), последний вывод: «…500 рун…»"`
      (обычная ошибка, не UpstreamError → 502);
    - ненулевой код выхода → `UpstreamError{502, "<утилита> завершилась с ошибкой (exit …): <хвост>"}`;
    - успех → `finish_reason: stop`.

**Важно:** у адаптера **нет собственного таймаута** и **нет ввода в stdin**. Если утилита задала
интерактивный вопрос, она будет висеть, пока не отменят контекст запроса (клиент закроет
соединение или истечёт таймаут вызывающей стороны). Вопрос попадёт в текст ошибки.

### 6.2. Шаблон команды: `{model}` и `{prompt}`

`Provider.Command` — JSON-массив аргументов (колонка `providers.command`, по умолчанию `[]`).
В **каждом** аргументе подстроки `{model}` и `{prompt}` заменяются целиком
(`strings.NewReplacer`), без участия shell — поэтому пробелы, кавычки и спецсимволы в модели и
промпте безопасны (тест `TestAntigravityPresetKeepsModelWithSpaces`: модель
`"Claude Opus 5.5 (Thinking)"` остаётся одним аргументом). Обязателен хотя бы один `{prompt}`,
иначе ошибка конфигурации (500). `{model}` необязателен — у `amp` и `goose` модель выбирается
самой утилитой.

Промпт передаётся **аргументом командной строки**: это ограничено `ARG_MAX` (≈1 МиБ на macOS
вместе с окружением) и виден в `ps` другим процессам того же пользователя (см. грабли).

### 6.3. Очистка вывода: `TermSanitizer` (`sanitize.go`)

Состояние: `pending` (незавершённый ESC или UTF-8 хвост прошлого чтения) и `line` (текущая
строка, ещё не отданная). Алгоритм `Write` (`sanitize.go:27`):
1. Склеить `pending + chunk`.
2. Если последний `ESC` начинает незавершённую последовательность (`escComplete`) — отложить её.
3. Если в конце разорванный многобайтовый UTF-8 символ (`incompleteUTF8Suffix`) — отложить.
4. Удалить OSC (`ESC ] … BEL|ESC \`), затем CSI (`ESC [ параметры финальный-байт`), затем прочие
   двухсимвольные `ESC X` (`stripANSI`).
5. `\r\n` → `\n`.
6. Посимвольно: `\n` — отдать строку (с обрезанными пробелами справа) + `\n`; одиночный `\r` —
   **сбросить строку** (так прогресс-бары и спиннеры перезаписывают себя); `\b` — удалить
   последнюю руну; `\t` — сохранить; кадры спиннеров-Брайля `U+2800–U+28FF`, `RuneError` и прочие
   управляющие символы — выбросить.

`Pending()` — незавершённая строка без изменения состояния; `Flush()` — отдать остаток и
очистить. Следствие построчной модели: клиент видит текст **строками**, а не токенами.

### 6.4. Заголовки, влияющие на CLI

| Заголовок | Константа | Значение |
|---|---|---|
| `X-Edy-Client` | `ClientHeader` (`server.go:32`) | Подпись агента для учёта расходов (`usage.client`). На все kind. |
| `X-Edy-Workdir` | `WorkdirHeader` (`cli.go:51`) | Рабочая папка процесса CLI (обычно корень проекта или git worktree эпика). Не проверяется. |
| `X-Edy-MCP` | `MCPHeader` (`climcp.go:17`) | Адрес `/mcp` этого же Edy (например, `http://127.0.0.1:11435/mcp?project=…`). Разрешены только схема `http`, путь ровно `/mcp`, хосты `127.0.0.1`, `localhost`, `::1` (`checkMCPURL`, `climcp.go:98`) — чтобы токен Edy не утёк на сторонний сервер. Только для `cli`-провайдеров, утилита которых есть в `mcpCapable`. |
| `X-Edy-MCP-Servers` | `MCPServersHeader` (`climcp.go:22`) | Имена внешних MCP-серверов из каталога Edy через запятую. Учитывается **только вместе с** `X-Edy-MCP`. Каждое имя должно быть в каталоге и включено (`extraMCP`, `climcp.go:111`), иначе 400. Секреты серверов берутся из каталога (в БД зашифрованы vault), в заголовке только имена. |

Эти заголовки ставит клиент агента `internal/agent/llm.go:113-122`.

### 6.5. MCP для Claude Code (`climcp.go`)

`mcpCapable` (`climcp.go:38`) — карта «basename исполняемого файла → функция флагов». Сейчас
там только `"claude": claudeMCP`. `SupportsMCP(command)` (`climcp.go:43`) смотрит на
`filepath.Base(command[0])`, поэтому подходят и `claude`, и `/usr/local/bin/claude`, но **не**
обёртки вроде `npx claude` или скрипт с другим именем.

`claudeMCP` (`climcp.go:55-94`):
1. Строит конфиг:
   ```json
   {"mcpServers": {
     "edy":   {"type": "http", "url": "<X-Edy-MCP>", "headers": {"Authorization": "Bearer <токен OmniRoute>"}},
     "<имя>": {"type": "http", "url": "...", "headers": {...}}            // внешний HTTP
     "<имя>": {"type": "stdio", "command": "...", "args": [...], "env": {...}}  // внешний stdio
   }}
   ```
   (имя `edy` запрещено для серверов каталога валидацией `store.MCPServer.Validate`,
   `internal/store/mcpservers.go:58`, так что перезаписать встроенный сервер нельзя).
2. Пишет его во временный файл `edy-mcp-*.json` в `os.TempDir()` с правами `0600` (в нём
   токен Edy и секреты внешних серверов); файл удаляется после завершения процесса (`cleanup`).
3. Возвращает аргументы:
   ```
   --mcp-config <файл> --strict-mcp-config
   --allowedTools mcp__edy [mcp__<внешний>...]
   --disallowedTools Bash Edit Write MultiEdit NotebookEdit
   ```
   `--strict-mcp-config` — Claude Code не подхватывает MCP-серверы из своих пользовательских
   настроек. `--allowedTools` — разрешение без вопросов на инструменты Edy и выданных серверов.
   `--disallowedTools` — свои инструменты правки файлов и терминал Claude Code выключены: все
   изменения идут через инструменты Edy (снимки Shadow Git, журнал), тесты — через `run_checks`.
   Чтение (`Read`, `Grep`, `Glob`) и веб-поиск остаются у Claude Code.

Итоговая командная строка для пресета `claude-code` с картинкой и MCP выглядит так:
```
claude --mcp-config /tmp/edy-mcp-123.json --strict-mcp-config --allowedTools mcp__edy mcp__github \
       --disallowedTools Bash Edit Write MultiEdit NotebookEdit \
       --add-dir /tmp/edy-images-456 -p --model <model> "<prompt>"
```

Как добавить MCP для другой утилиты: написать функцию `mcpFlags` (вернуть аргументы, функцию
уборки, ошибку) и добавить её в `mcpCapable`; тег `mcp` в `Caps()` появится автоматически.
Остальные утилиты настраивают MCP в своих файлах, поэтому Edy работает с ними через эмуляцию
инструментов.

### 6.6. Картинки для CLI (`cliimages.go`)

`materializeImages` (`cliimages.go:22`) проходит по всем сообщениям:
- `input_audio` → ошибка «CLI-утилиты не принимают звук» (400);
- `image_url` с `data:image/(png|jpeg|gif|webp);base64,…` → файл `image-N.<ext>` (`0600`) во
  временной папке `edy-images-*`, а в тексте сообщения часть заменяется на
  `[Изображение: <путь> — открой файл, чтобы посмотреть]`;
- `image_url` с обычной ссылкой → текст `[Изображение по ссылке: <url>]` (файл не скачивается);
- другой MIME или не base64 → ошибка 400; битый base64 → 400.

Изменённое сообщение пересобирается в **строку** (все текстовые части через `\n`). Папка
удаляется после завершения процесса. Для `claude` добавляется `--add-dir <папка>` (`addDirFlag`,
`cliimages.go:14`), потому что картинки лежат вне рабочей папки. Перед этим сервер уже проверил
тег `vision` (у `claude` он есть по умолчанию, у неизвестных утилит — нет).

---

## 7. Теги возможностей (`caps.go`)

Теги описывают, что провайдер умеет **через Edy**. Они хранятся на уровне провайдера (колонка
`providers.caps`), а не модели.

| ID | Константа | Название в UI | Подсказка | На что влияет в коде |
|---|---|---|---|---|
| `mcp` | `CapMCP` | MCP | подключает MCP Edy и внешние MCP-серверы; скиллы получает через MCP | Оркестратор может выдать исполнителю MCP Edy и внешние серверы (`internal/api/mcpapi.go:229`, `internal/api/projects.go:324`). **Нельзя включить вручную**: `Caps()` оставляет его, только если kind=`cli` и `SupportsMCP(command)`. |
| `tools` | `CapTools` | Инструменты | родной вызов функций (без эмуляции в промпте) | Только информация для Оркестратора/UI. На эмуляцию **не влияет** (она зависит от kind). |
| `vision` | `CapVision` | Картинки | понимает изображения и скриншоты | Проверка в `handleChat` (`server.go:163`); `Tasks.CanSee` — отдавать ли агенту скриншоты/вложения (`internal/app/app.go:123-127`). |
| `audio` | `CapAudio` | Звук | понимает аудио и речь | Проверка в `handleChat` (`server.go:167`). |
| `voice` | `CapVoice` | Голос | отвечает голосом | Только информация (нигде не используется в логике). |
| `image_gen` | `CapImageGen` | Рисует | создаёт изображения | Только информация. |
| `web` | `CapWeb` | Интернет | сам ищет в интернете | Только информация. |
| `long_context` | `CapLongContext` | Длинный контекст | держит в голове большой проект целиком | Только информация. |

«Только информация» = названия тегов (`CapTitle`) передаются Оркестратору в описании
исполнителей (`internal/api/projects.go:325-326`), и он учитывает их при выборе модели.

**Умолчания** — `DefaultCaps(p)` (`caps.go:54`), используются, пока пользователь не задал теги
(`Provider.Caps == nil`):

| kind / утилита (`basename(Command[0])`) | Теги по умолчанию |
|---|---|
| `anthropic` | tools, vision, long_context |
| `openai` | tools |
| `cli` + `claude` | mcp, tools, vision, web, long_context |
| `cli` + `agy`, `gemini`, `qwen` | tools, web, long_context |
| `cli` + `codex`, `copilot`, `opencode`, `cursor-agent`, `agent` | tools, web |
| `cli` + прочие (`amp`, `goose`, `aider`, `ollama`, `llm`, свои) или пустая команда | — |
| `web` | web |

**Действующие теги** — `Caps(p)` (`caps.go:81`): берёт `p.Caps` или умолчания, убирает дубли,
неизвестные ID (у которых `CapTitle(id) == id`) и `mcp`, если утилита его технически не умеет.
`HasCap(p, c)` (`caps.go:100`) — проверка по `Caps(p)`.

**Хранение.** `store.SetCaps(name, caps)` (`store.go:171`): `nil` → пустая строка в БД
(автоопределение), иначе JSON-массив. При чтении (`scanProvider`, `store.go:231-234`) пустая
строка → `nil`, непустая → `[]string{}` + `json.Unmarshal` (ошибка разбора молча игнорируется —
получится пустой список). `AddProvider` теги **не** сохраняет — после добавления они всегда
автоматические. UI: `GET /api/caps` отдаёт `CapCatalog`, установка тегов — через API
(`internal/api/api.go:311-337`, см. [05-api.md](05-api.md)).

---

## 8. Каталог пресетов

Пресеты используются при добавлении провайдера в UI и в `edy provider add -preset …`
(`internal/api/api.go:190-193,411,548`, `cmd/edy/main.go:189`). Команду установки UI/CLI выполняет
через bash **после подтверждения пользователя** (`ensureInstalled`, `cmd/edy/main.go:231`).
Тест `TestPresetsHaveInstallAndPrompt` требует у каждого CLI-пресета команду установки и `{prompt}`.

### 8.1. CLI-пресеты (`CLIPresets`, `presets.go:17-95`)

| Ключ | Название | Команда (`Command`) | Установка (`Install`) | Заметка |
|---|---|---|---|---|
| `claude-code` | Claude Code (Anthropic) | `claude -p --model {model} {prompt}` | `curl -fsSL https://claude.ai/install.sh \| bash` | По подписке Pro/Max; если задан `ANTHROPIC_API_KEY`, используется он. Единственный с MCP. |
| `antigravity` | Google Antigravity | `agy -p {prompt} --model {model}` | `curl -fsSL https://antigravity.google/cli/install.sh \| bash` | Gemini и Claude по подписке Google; имена моделей — `agy models`. |
| `codex` | OpenAI Codex CLI | `codex exec -m {model} {prompt}` | `npm install -g @openai/codex` | Подписка ChatGPT или ключ; Node.js 22+. |
| `gemini` | Gemini CLI (Google) | `gemini -m {model} {prompt}` | `npm install -g @google/gemini-cli` | Для личных подписок заменён Antigravity; с ключом работает. |
| `qwen-code` | Qwen Code (Alibaba) | `qwen -m {model} -p {prompt}` | `npm install -g @qwen-code/qwen-code` | — |
| `cursor` | Cursor CLI | `agent -p --model {model} {prompt}` | `curl https://cursor.com/install -fsS \| bash` | Без `--force` только предлагает правки. |
| `copilot` | GitHub Copilot CLI | `copilot -p {prompt} --model {model}` | `npm install -g @github/copilot` | Нужна подписка Copilot. |
| `opencode` | OpenCode | `opencode run -m {model} {prompt}` | `curl -fsSL https://opencode.ai/install \| bash` | Модель как `провайдер/модель`. |
| `amp` | Amp (Sourcegraph) | `amp -x {prompt}` | `npm install -g @sourcegraph/amp` | Модель выбирает Amp, `{model}` нет. |
| `goose` | Goose (Block) | `goose run -t {prompt}` | `brew install block-goose-cli` | Модель — в `goose configure`. |
| `aider` | Aider | `aider --model {model} --yes-always --no-pretty --message {prompt}` | `curl -LsSf https://aider.chat/install.sh \| sh` | Сам правит файлы и коммитит. |
| `ollama` | Ollama (локальные модели) | `ollama run {model} --nowordwrap {prompt}` | `brew install ollama` | Для стриминга лучше API `http://localhost:11434/v1`. |
| `llm` | LLM (Simon Willison) | `llm -m {model} {prompt}` | `brew install llm` | Плагины для десятков провайдеров. |

Флаги неинтерактивного режима «сверены с документацией утилит» (комментарий `presets.go:14-16`),
но на реальных установках в рамках этого кода не проверялись автоматически. Если у пользователя
другая версия, провайдера можно добавить со своей командой.

### 8.2. API-пресеты (`APIPresets`, `presets.go:107-128`)

| Ключ | Название | kind | Base URL | Заметка |
|---|---|---|---|---|
| `anthropic` | Anthropic (Claude) | anthropic | (по умолчанию SDK) | — |
| `openai` | OpenAI | openai | `https://api.openai.com/v1` | — |
| `google` | Google Gemini API | openai | `https://generativelanguage.googleapis.com/v1beta/openai` | имена моделей приходят с `models/` — срезается в `ListModels` |
| `openrouter` | OpenRouter (сотни моделей) | openai | `https://openrouter.ai/api/v1` | один ключ — все крупные провайдеры |
| `deepseek` | DeepSeek | openai | `https://api.deepseek.com/v1` | — |
| `mistral` | Mistral AI | openai | `https://api.mistral.ai/v1` | — |
| `xai` | xAI (Grok) | openai | `https://api.x.ai/v1` | — |
| `groq` | Groq (очень быстрый) | openai | `https://api.groq.com/openai/v1` | — |
| `cerebras` | Cerebras | openai | `https://api.cerebras.ai/v1` | — |
| `together` | Together AI | openai | `https://api.together.xyz/v1` | — |
| `fireworks` | Fireworks AI | openai | `https://api.fireworks.ai/inference/v1` | — |
| `perplexity` | Perplexity | openai | `https://api.perplexity.ai` | модели с поиском |
| `moonshot` | Moonshot (Kimi) | openai | `https://api.moonshot.ai/v1` | — |
| `zai` | Z.ai (GLM) | openai | `https://api.z.ai/api/paas/v4` | — |
| `qwen` | Alibaba Qwen (DashScope) | openai | `https://dashscope-intl.aliyuncs.com/compatible-mode/v1` | — |
| `cohere` | Cohere | openai | `https://api.cohere.ai/compatibility/v1` | — |
| `ollama` | Ollama (локально) | openai | `http://localhost:11434/v1` | ключ не нужен |
| `lmstudio` | LM Studio (локально) | openai | `http://localhost:1234/v1` | ключ не нужен |
| `vllm` | vLLM (локально) | openai | `http://localhost:8000/v1` | ключ не нужен без `--api-key` |
| `llamacpp` | llama.cpp server (локально) | openai | `http://localhost:8080/v1` | ключ не нужен |

У каждого (кроме локальных) есть `KeyURL` — ссылка «где получить ключ» для UI. Совместимость
каждого провайдера со `stream_options.include_usage` и с tools не проверялась — это проверяется
только тестовым фейковым сервером.

### 8.3. Веб-сайты (`DefaultSites`, `internal/webchat/sites.go:43-74`)

| Ключ | Название | URL | Пауза | Лимит/день |
|---|---|---|---|---|
| `chatgpt` | ChatGPT | `https://chatgpt.com/` | 15 с | 150 |
| `claude` | Claude.ai | `https://claude.ai/new` | 15 с | 150 |
| `gemini` | Gemini | `https://gemini.google.com/app` | 15 с | 150 |

Селекторы — в разделе 9.4.

---

## 9. Веб-чаты (`internal/webchat`)

### 9.1. Принципы и запреты

Документация пакета (`internal/webchat/chrome.go:1-6`) формулирует принцип: **вести себя как
обычный человек, а не маскироваться**. Обычный Chrome/Chromium с видимым окном, ручной вход
пользователя, один запрос за раз с паузами и дневным лимитом.

> **Владелец проекта запрещает:** подмену отпечатков браузера (fingerprint spoofing), stealth-
> плагины и любые приёмы скрытия автоматизации (`navigator.webdriver` и т.п.), а также обход
> капчи (сервисы распознавания, автоклики по капче и пр.). Ничего из этого в коде нет, и
> добавлять это **нельзя**. Если сайт показывает капчу или блокирует автоматизацию — это сигнал
> пользователю пройти проверку вручную в окне входа или отказаться от веб-провайдера, а не задача
> для обхода. Также нельзя автоматизировать ввод пароля/2FA: вход выполняет только сам
> пользователь в окне без автоматизации.

Веб-адаптер помечен в `docs/ARCHITECTURE.md` как экспериментальная функция «на свой риск»
(нарушает ToS сервисов, селекторы часто ломаются).

### 9.2. Браузер и профили

- **Виды браузера** (`chrome.go:24-30`): `chromium` — изолированный Chromium, который Edy
  скачивает в `<home>/chromium`; `chrome` — установленный Google Chrome
  (`/Applications/Google Chrome.app/…`, `~/Applications/…`, `/usr/bin/google-chrome`), но с
  **отдельной** папкой профиля Edy, обычные вкладки и аккаунты пользователя не затрагиваются.
- **`FindBrowser(dataDir, kind)`** (`chrome.go:37`): переменная окружения `EDY_CHROME` подменяет
  путь для **любого** вида (для тестов).
- **Скачивание Chromium** (`DownloadChromium`, `chrome.go:83`): официальные snapshot-сборки
  `https://commondatastorage.googleapis.com/chromium-browser-snapshots/<Mac_Arm|Mac|Linux_x64>/LAST_CHANGE`
  → архив `<rev>/chrome-mac.zip|chrome-linux.zip` во временный файл в `dataDir`, распаковка
  `unzip` с сохранением прав и **симлинков** (без них `.app` не запускается) и защитой от
  `../` (zip slip). Поддержаны только `darwin/arm64`, `darwin/amd64`, `linux/amd64`. Проверки
  подписи/хеша нет. `Manager.EnsureBrowser` скачивает один раз под `downloadMu` даже при
  параллельных запросах. CLI: `edy browser install` (`cmd/edy/main.go:95-97`).
- **Профили**: `<home>/web-profiles/<имя провайдера>` (`profileDir`, `manager.go:160`), права
  `0700`. Имя проверяется `validProfile` (не пустое, не `.`/`..`, без `/` и `\`). У каждого
  профиля **свой процесс браузера**, своя пауза и лимит — к одному сайту можно подключить
  несколько аккаунтов (разные провайдеры). Cookies лежат в профиле **открытым** образом
  (стандартное хранилище Chromium), vault их не шифрует.
- **Аргументы запуска** (`baseArgs`, `manager.go:172`): `--user-data-dir=…`, `--no-first-run`,
  `--no-default-browser-check`, в тестах ещё `--headless=new` и `ExtraFlags`. Управляемый
  браузер дополнительно (`launch`, `manager.go:328-335`): `--remote-debugging-port=0` (порт
  выбирает браузер и пишет в `DevToolsActivePort`), `--disable-background-timer-throttling`,
  `--disable-backgrounding-occluded-windows`, `--disable-renderer-backgrounding` (чтобы ответ
  генерировался в свёрнутом окне), стартовая страница `about:blank` (аргументы собирает `launchArgs`). По умолчанию браузер
  **скрытый**: `--headless=new --window-size=1280,900`, тот же профиль, без окна и значка в Dock.
  Если у провайдера включено «Показывать окно» (`Conn.Visible` ← `Provider.ShowWindow`), окно
  видимое. При смене режима `ensure` перезапускает браузер. Окно входа (`Login`) всегда видимое.
  User-agent не подменяется: если сайт не пускает headless-браузер, включите «Показывать окно».
- **Подключение CDP** (`launch`): ждёт `DevToolsActivePort` до 30 с (`waitDevToolsURL`); если
  процесс сразу завершился — «возможно, этот профиль уже открыт в другом окне»; оставляет ровно
  одну вкладку (`singlePage` через `/json/list` и `/json/close/<id>`, закрывая восстановленные) и
  подключается к ней `chromedp.WithTargetID` (без этого chromedp открыл бы лишние вкладки).
- **Живучесть** (`ensure`, `manager.go:288`): перед каждым запросом `alive()` (процесс жив и CDP
  отвечает за 5 с); иначе браузер перезапускается, в `Log` пишется сообщение. Тест
  `TestPolitenessAndWatchdog` убивает браузер и проверяет перезапуск.
- **Остановка** (`stop`, `killGroup`): отмена контекстов chromedp, `SIGKILL` всей группе процессов
  (браузер стартует с `Setpgid`), ожидание до 5 с + 300 мс на дочерние процессы.
  `Manager.Close()` закрывает все управляемые браузеры, **окна входа не трогает**.
  `RemoveProfile` закрывает браузер и окно входа, удаляет статус и папку профиля.

### 9.3. Вход в аккаунт (login flow)

`Manager.Login(ctx, conn)` (`manager.go:187-233`):
1. Проверка профиля и сайта, `EnsureBrowser` (может скачать Chromium).
2. Если окно входа уже открыто — ничего не делать. Если для профиля работает управляемый
   браузер — **закрыть его** (профиль не может быть открыт двумя процессами).
3. Запустить браузер **без** `--remote-debugging-port` и без какой-либо автоматизации — обычное
   окно с `s.URL`. Процесс запоминается в `m.logins[profile]`.
4. Пользователь сам входит (пароль, 2FA, капча — всё вручную) и **закрывает окно**.
5. Горутина ждёт завершения процесса, убирает его из `logins` и вызывает `Check` с таймаутом
   2 минуты.

Пока окно входа открыто, `Status` возвращает `login_open`, а `Ask`/`Check` получают
`ErrLoginOpen`. UI/CLI опрашивают статус (`cmd/edy/main.go:278-291` — CLI ждёт, пока статус не
станет `ok`/`login_required`/`error`).

### 9.4. Сайты и селекторы (`sites.go`)

`Site` (`sites.go:15-30`): `Title`, `URL` (открывает **новый** пустой чат), `Input` (поле ввода:
textarea или contenteditable), `Send` (кнопка отправки; необязательна), `Response` (блок ответа,
берётся **последний**), `Busy` (элемент, видимый во время генерации, обычно кнопка «Стоп»),
`MinIntervalSec`, `DailyLimit`. Каждый селектор — CSS-список через запятую: если сайт поменял
вёрстку, срабатывает запасной вариант.

Встроенные селекторы (`sites.go:44-73`), **на реальных аккаунтах не проверены** (так и написано
в комментарии `sites.go:35-41`):

| Сайт | Input | Send | Response | Busy |
|---|---|---|---|---|
| chatgpt | `#prompt-textarea, div.ProseMirror[contenteditable='true'], form textarea` | `button[data-testid='send-button'], #composer-submit-button, button[data-testid='composer-submit-button']` | `[data-message-author-role='assistant'] .markdown, [data-message-author-role='assistant']` | `button[data-testid='stop-button'], button[aria-label*='Stop' i], button[aria-label*='Останов' i]` |
| claude | `div.ProseMirror[contenteditable='true'], div[contenteditable='true']` | `button[aria-label='Send message'], button[aria-label='Send Message'], button[aria-label*='Отправ' i]` | `.font-claude-response, .font-claude-message, [data-is-streaming] .standard-markdown, [data-is-streaming]` | `[data-is-streaming='true'], button[aria-label*='Stop' i], button[aria-label*='Останов' i]` |
| gemini | `rich-textarea .ql-editor, rich-textarea div[contenteditable='true'], div.ql-editor[contenteditable='true']` | `button.send-button, button[aria-label*='Send' i], button[aria-label*='Отправ' i]` | `message-content .markdown, model-response message-content, message-content` | `button.send-button.stop, button[aria-label*='Stop' i], button[aria-label*='Остановить' i]` |

**Переопределение без пересборки**: файл `<home>/web-sites.json` (`SitesFile`, `sites.go:77`) —
JSON-объект `{"<ключ>": {Site}}`. `LoadSites` (`sites.go:81`) перечитывает его **на каждом**
запросе: встроенные сайты дополняются (поле переносится, только если оно непустое/положительное
— `merge`, `sites.go:109`), можно добавить новый сайт. После слияния у каждого сайта должны быть
`url`, `input`, `response`, иначе ошибка. Пример:
```json
{
  "chatgpt": {"response": "[data-message-author-role='assistant'] .prose"},
  "mychat":  {"title": "Мой чат", "url": "https://chat.example/new", "input": "textarea",
              "response": ".answer", "busy": ".stop", "min_interval_sec": 30, "daily_limit": 50}
}
```
Необязательные поля `upload` (поле выбора файла для картинок) и `images` (картинки в ответе)
задаются так же; пустые — используются общие признаки (см. 9.5).

Нулевые `min_interval_sec`/`daily_limit` в переопределении **не** сбрасывают встроенные значения
(их нельзя выключить через файл); у нового сайта нули означают «без паузы» и «без лимита».

### 9.5. Отправка сообщения: `Ask`

`Manager.Ask(ctx, conn, prompt, images, onText)` (`manager.go:744`):
1. Проверка профиля, загрузка сайта.
2. `acquire` (`manager.go:529`) — **вежливость**: мьютекс профиля (один запрос/проверка за
   раз, ожидание прерывается контекстом); сброс счётчика при смене даты; если
   `DailyLimit > 0 && count >= DailyLimit` → ошибка «дневной лимит запросов к X исчерпан (N) — так
   меньше риск блокировки»; ожидание `MinIntervalSec` от **окончания** прошлого запроса;
   `count++` (засчитывается и неудачный запрос). Освобождение ставит `lastDone = now`.
   Счётчики только в памяти — перезапуск Edy их обнуляет.
3. `withTab` — живой браузер профиля (`ensure`) и функция `run(timeout, actions…)`, отменяемая
   и по таймауту, и по `ctx`.
4. `ask` (`manager.go:694-820`):
   - `Navigate(s.URL)` (до 60 с) — каждый запрос в **новом** чате;
   - ждёт поле ввода до 30 с (опрос каждые 500 мс скриптом `inputJS`); нет поля →
     `ErrLoginRequired` «…на странице нет поля ввода. Нажмите «Войти» у провайдера в Edy»;
   - снимок состояния `before` (число блоков ответа, текст последнего, busy) — `stateJS`;
   - **вставка**: `focusJS` (фокус и курсор в конец, для contenteditable — через Range), затем
     CDP `Input.insertText(prompt)` — текст целиком, как из буфера обмена (без посимвольной
     «печати»). Если поле осталось пустым — запасной `document.execCommand("insertText")`
     (`insertFallbackJS`; нужен, например, Quill в Gemini). Опять пусто — ошибка;
   - **отправка**: сначала `Enter` (`chromedp.KeyEvent(kb.Enter)`), пауза 1,2 с; «отправлено»
     = поле опустело **или** появился новый блок ответа **или** видим `Busy`. Если нет — клик по
     первой видимой и активной кнопке из `s.Send`, затем из общих признаков `genericSend`
     (`manager.go:640`: `data-testid*="send"`, `aria-label*="send"`/`"отправ"`,
     `type="submit"`), пауза 1,5 с, повторная проверка; не вышло — ошибка «текст в поле, но
     отправить его не получилось…»;
   - **ожидание ответа**: опрос каждые 400 мс; пока не появился новый блок/текст — ждать до 90 с
     (иначе ошибка «селектор ответа (response) устарел»); при каждом изменении текста —
     `onText(полный текст)`; конец — когда `!busy`, текст не пустой и не менялся 1,5 с; общий
     предел — 15 минут.
   - Текст ответа берётся как `innerText` последнего «листового» элемента под селектором
     `Response` (`leaves` отбрасывает элементы, содержащие другие совпадения), то есть
     **отрисованный текст, а не markdown** — форматирование кода и списков частично теряется.
5. `record` переводит результат в статус (`ok`, `login_required`, `error`; `ErrLoginOpen` и
   отмена статус не меняют).

**Картинки (пункт 1.12).** `images` — пути к временным файлам: `WebAsker.Ask`
(`omniroute/web.go`) раскладывает data URL из `image_url` через `writeImages`
(`cliimages.go`), а в текст промпта на месте каждой картинки ставит подпись
«[Изображение N — во вложении]». В `ask` после появления поля ввода и до вставки текста
вызывается `attach` (`manager.go:943`):
- ищет поле выбора файла по `Upload` (по умолчанию `input[type='file']`, скрипт `pickUploadJS`
  помечает его `data-edy-upload`) и прикладывает файлы через CDP `SetUploadFiles`;
- поля нет — запасной путь: файлы вставляются в поле ввода синтетическим `paste` с
  `DataTransfer` (`pasteFilesJS`); не принял и он — ошибка «…задайте селектор upload»;
- затем пауза 2 с и ожидание до 60 с, пока на странице видны признаки загрузки
  (`uploadingJS`: видимые `[role=progressbar]` и `<progress>`).

Промпт может быть пустым, если есть картинки. **Картинки ответа**: при каждом опросе
`fetchImagesJS` собирает `<img>` внутри блока ответа (или по селектору `Images`) шириной от
128 px — иконки и аватарки отсекаются. Новые картинки `grabImages` (`manager.go:988`)
скачивает `fetch` с куками сайта и превращает в data URL; если сайт не отдаёт (CORS) —
снимок элемента (`markImageJS` + `chromedp.Screenshot`). В текст ответа картинка попадает как
markdown `![image](data:image/...)`; UI (`renderText` в `index.html`) показывает её как
`<img>` (принимаются только `data:image/…;base64` и `https://`). На настоящих ChatGPT/Gemini
загрузка и выдача картинок пока не проверены — при поломке правьте `upload`/`images` в
`web-sites.json`.

### 9.6. Проверка и статусы

`Check(ctx, conn)` (`manager.go:237`): ставит `checking`, занимает профиль (без паузы и лимита),
открывает сайт и до 20 с ждёт поле ввода → `ok` или `login_required`. Возвращает статус.

Состояния (`manager.go:41-48`): `unknown` (не проверялось с запуска), `login_open`, `checking`,
`ok`, `login_required`, `error`. `Status` (`manager.go:51`) = `{state, message, at}`; хранится
в памяти. `Status(profile)` возвращает `login_open`, если окно входа открыто, иначе последнее
записанное состояние. В API интерфейса доступны `Sites`, `Login`, `Check`, `Status`,
`RemoveProfile`, `BrowserInstalled` (интерфейс `internal/api/api.go:33-38`).

### 9.7. Диагностика

`fail(tab, profile, err)` (`manager.go:824`) при любом сбое на странице сохраняет в
`<home>/web-debug/<profile>-<YYYYMMDD-HHMMSS>`:
- `.jpg` — полный скриншот страницы (качество 70);
- `.json` — дамп `dumpJS`: `url`, `title`, до 20 полей ввода, до 100 видимых кнопок, последние
  40 «похожих на сообщения» блоков с атрибутами `id`, `class`, `role`, `aria-label`,
  `data-testid`, `contenteditable`, `type`, `placeholder`, `data-message-author-role`,
  `data-is-streaming` и первыми 100 символами текста.

Пути дописываются к ошибке: `… (диагностика: /…/web-debug/…jpg, …json)`. По JSON удобно
подобрать новые селекторы и положить их в `web-sites.json`. **Внимание:** скриншоты содержат
переписку пользователя и не удаляются автоматически.

### 9.8. Что НЕ реализовано

- Загрузка файлов, кроме картинок; выбор модели на сайте; продолжение существующего чата
  (каждый запрос — новый чат), удаление созданных чатов.
- Нативные tools (только эмуляция через текст), реальные usage-токены.
- Сохранение статусов и дневных счётчиков между перезапусками.
- Автоматическое обновление селекторов, проверка селекторов на реальных аккаунтах.
- И, сознательно, — никакой маскировки автоматизации, подмены отпечатков, stealth-плагинов,
  обхода капчи и автоматического входа (см. 9.1).

---

## 10. Vault: шифрование секретов

`internal/vault/vault.go`:
- **Алгоритм**: AES-256-GCM (`crypto/aes` + `cipher.NewGCM`), мастер-ключ ровно 32 байта
  (`New`, `vault.go:29`). `Seal` (`vault.go:68`) генерирует случайный 12-байтный nonce и
  возвращает `nonce || ciphertext||tag`; `Open` (`vault.go:77`) проверяет длину и тег —
  подменённые данные дают ошибку. AAD не используется.
- **Мастер-ключ** — `FromKeychain()` (`vault.go:46`): `go-keyring` (на macOS — Keychain,
  сервис `dev.edy.ide`, аккаунт `master-key`, `vault.go:18-19`); значение — base64 от 32 случайных
  байт. Нет записи → генерируется и сохраняется; ошибка Keychain/повреждённый base64 — ошибка
  запуска. Мастер-ключ **никогда не попадает в БД**. Вызывается в `app.OpenStore`
  (`internal/app/app.go:52`).
- **Что шифруется**: API-ключи провайдеров (`api_keys.secret`, `store.go:248`), токены git-хостингов
  (`internal/store/git.go:39`), конфигурации MCP-серверов каталога с секретами
  (`internal/store/mcpservers.go:82`).
- **Что НЕ шифруется**: токен OmniRoute (`settings.router_token` и файл `router.token`, `0600`),
  cookies веб-профилей (`web-profiles/`), остальные настройки, история расходов. Комментарий
  пакета упоминает «cookies», но фактически vault их не касается.
- **Что никогда не возвращается/не логируется**: API интерфейса отдаёт ключи только через
  `KeyInfos` (`id`, `label`, `disabled`, без секрета) — тест `TestProviderAndKeyLifecycleNeverLeaksSecrets`
  (`internal/api/api_test.go:49`); OmniRoute в логах пишет только `key.Label`, не секрет
  (`server.go:285,290`); тело ошибки провайдера (`UpstreamError.Message`) возвращается клиенту
  — провайдеры обычно не включают туда ключ, но OmniRoute это не фильтрует. Временный MCP-конфиг
  с токеном пишется с правами `0600` и удаляется. Расшифрованный ключ живёт только в памяти на
  время запроса (`Call.Key`).
- **Потеря Keychain-записи** (переустановка, другой Mac, сброс связки) = все зашифрованные
  секреты нечитаемы; `store.Keys` тогда возвращает ошибку «не удалось расшифровать ключ N» и
  запросы к провайдеру падают целиком. Восстановление — удалить ключи и добавить заново.
  Резервного экспорта мастер-ключа нет.

---

## 11. Хранилище: провайдеры, ключи, расходы, бюджет, настройки

Схема и миграции — в [06-store.md](06-store.md). Здесь — что важно для OmniRoute.

**`providers`** (`store.go:31-39` + миграции `store.go:66-69`): `id`, `name` (UNIQUE — префикс
модели), `kind`, `base_url`, `models` (строка через запятую), `price_in`, `price_out` (USD за 1M
токенов), `command` (JSON-массив, по умолчанию `[]`), `site`, `browser`, `caps` (`''` = авто,
иначе JSON-массив).

**`api_keys`** (`store.go:40-46`): `provider_id` (FK с `ON DELETE CASCADE` — ключи удаляются
вместе с провайдером), `label`, `secret` (BLOB, шифротекст vault), `disabled` (0/1).

**`settings`** (`store.go:47-50`): ключ–значение. Для этой области: `daily_budget_usd`
(`omniroute.SettingDailyBudget`), `router_token` (`internal/app/app.go:31`).

**`usage`** (`store.go:51-61`): `ts` (unix-секунды, `time.Now()` в момент записи), `provider`,
`model`, `client`, `input_tokens`, `output_tokens`, `cost_usd`; индекс по `ts`.

| Метод | Строка | Что делает |
|---|---|---|
| `Provider.Cost(in, out)` | `store.go:95` | `(in*PriceIn + out*PriceOut)/1e6` |
| `AddProvider(p)` | `store.go:142` | Вставляет провайдера (без `caps`); дубль имени → `ErrExists` |
| `SetModels(name, models)` | `store.go:159` | Заменяет список моделей |
| `SetCaps(name, caps)` | `store.go:171` | Теги вручную; `nil` → автоопределение |
| `DeleteProvider(name)` | `store.go:188` | Удаляет провайдера и (каскадом) ключи |
| `Provider(name)` / `Providers()` | `store.go:199` / `208` | Чтение (`ErrNotFound`), список по имени |
| `AddKey(providerID, label, secret)` | `store.go:247` | Шифрует vault и сохраняет ключ |
| `Keys(providerID)` | `store.go:260` | Включённые ключи, расшифрованные, в порядке добавления |
| `KeyInfos(providerID)` | `store.go:291` | Все ключи без секретов (для UI) |
| `DeleteKey(id)` | `store.go:308` | Удаляет ключ по id |
| `DisableKey(id)` | `store.go:320` | `disabled = 1` (после 401/403) |
| `Setting(key)` / `SetSetting(key, v)` | `store.go:325` / `334` | Настройки (upsert) |
| `RecordUsage(u)` | `store.go:339` | Пишет строку расхода |
| `UsageSince(t)` | `store.go:357` | Сводка по `client, provider, model`, дорогие первыми |
| `SpentSince(t)` | `store.go:376` | Сумма `cost_usd` с момента `t` (для бюджета) |

---

## 12. Справочник: все объявления по файлам

Экспортированные объявления выделены; неэкспортированные приведены, когда они важны для понимания.

### `internal/omniroute/server.go`

| Объявление | Строка | Описание |
|---|---|---|
| **`DefaultAddr`** | 25 | `127.0.0.1:11435` — адрес по умолчанию |
| **`SettingDailyBudget`** | 28 | Имя настройки дневного лимита `daily_budget_usd` |
| **`ClientHeader`** | 32 | `X-Edy-Client` — подпись агента для учёта расходов |
| `defaultCooldown`, `maxBodyBytes` | 34-35 | Перерыв ключа 1 мин; лимит тела 32 МиБ |
| **`Server`** | 39 | HTTP-обработчик OmniRoute (store, адаптеры, токен, лог, часы, перерывы ключей) |
| **`NewServer(st, token, log)`** | 52 | Создаёт сервер с адаптерами openai/anthropic/cli |
| **`(*Server).RegisterAdapter(kind, a)`** | 72 | Подключает/заменяет адаптер для kind (web) |
| **`NewToken()`** | 77 | Случайный токен `edy-<48 hex>` |
| **`(*Server).ServeHTTP`** | 85 | Маршрутизация и авторизация |
| `authorized` | 106 | Проверка `Bearer` за постоянное время |
| `handleModels` | 111 | `GET /v1/models` |
| `handleChat` | 131 | `POST /v1/chat/completions` |
| `checkBudget` | 227 | Текст ошибки, если бюджет исчерпан |
| `callWithRotation` | 253 | Перебор ключей с перерывами и отключением |
| `coolingDown` / `coolDown` | 301 / 308 | Проверка и установка перерыва ключа |
| `responder`, `newResponder` | 318 / 331 | Запись ответа клиенту |
| `emit` / `send` / `mergeToolCalls` / `finish` / `fail` | 337 / 362 / 377 / 397 / 424 | См. 3.9 |
| `writeJSON` / `writeError` | 442 / 448 | JSON-ответ / ошибка в формате OpenAI |
| `mediaIn` | 453 | Есть ли в сообщениях картинки/звук |

### `internal/omniroute/adapter.go`

| Объявление | Строка | Описание |
|---|---|---|
| **`Event`** | 15 | Дельта ответа + `FinishReason` (один раз в конце) |
| **`Call`** | 21 | Один вызов модели: провайдер, ключ, модель без префикса, запрос, рабочая папка, MCP |
| **`Adapter`** | 33 | Интерфейс `Stream(ctx, call, emit) (Usage, error)` |
| **`UpstreamError`** | 38 | HTTP-ошибка провайдера до начала генерации (`Status`, `RetryAfter`, `Message`) |
| **`(*UpstreamError).Error`** | 44 | «провайдер ответил N: …» |
| `retryAfter` | 48 | Разбор `Retry-After` (секунды или дата) |

### `internal/omniroute/types.go`

| Объявление | Строка | Описание |
|---|---|---|
| **`ChatRequest`** | 13 | Входящий запрос OpenAI + `Raw` (исходное тело) |
| `parseChatRequest` | 25 | Разбор и валидация (model, messages обязательны) |
| `splitModel` | 44 | `провайдер/модель` по первому `/` |
| **`Message`** | 49 | Сообщение: role, content (строка или части), name, tool_calls, tool_call_id |
| **`ContentPart`** | 57 | Часть сообщения: text / image_url / input_audio |
| **`InputAudio`** | 65 | Звук: base64 + формат |
| **`ImageURL`** | 70 | URL картинки (http(s) или data:) |
| **`(Message).Parts`** | 76 | Содержимое как список частей (строка → одна текстовая часть) |
| **`(Message).Text`** | 92 | Склейка текстовых частей |
| **`Tool`**, **`ToolFunction`** | 106 / 111 | Описание инструмента |
| **`ToolCall`**, **`FunctionCall`** | 117 / 124 | Вызов инструмента (с `Index` для стрима) |
| **`Usage`** | 129 | Токены prompt/completion/total |
| **`Delta`** | 136 | Приращение: role, content, tool_calls |
| **`Chunk`**, **`ChunkChoice`** | 143 / 152 | SSE `chat.completion.chunk` |
| **`Completion`**, **`CompletionChoice`**, **`CompletionMessage`** | 159 / 168 / 174 | Нестриминговый ответ |

### `internal/omniroute/openai.go`

| Объявление | Строка | Описание |
|---|---|---|
| `defaultOpenAIBaseURL` | 14 | `https://api.openai.com/v1` |
| **`OpenAIAdapter`** | 19 | Адаптер OpenAI-совместимых API (`HTTP` — свой клиент, nil → default) |
| **`(*OpenAIAdapter).Stream`** | 23 | Проброс тела + SSE → Event |

### `internal/omniroute/anthropic.go`

| Объявление | Строка | Описание |
|---|---|---|
| `defaultAnthropicMaxTokens` | 17 | 32000, если клиент не передал max_tokens |
| **`AnthropicAdapter`** | 22 | Адаптер Anthropic Messages API (`HTTP` — свой клиент) |
| **`(*AnthropicAdapter).Stream`** | 26 | Перевод запроса, стрим SDK → Event |
| `finishReason` | 110 | stop_reason → finish_reason |
| `toAnthropicParams` | 127 | OpenAI-запрос → `MessageNewParams` |
| `userBlocks` | 226 | Части user-сообщения → блоки |
| `imageBlock` | 257 | data:/http картинка → image block |
| `toInputSchema` | 270 | JSON Schema → `ToolInputSchemaParam` |

### `internal/omniroute/cli.go`

| Объявление | Строка | Описание |
|---|---|---|
| `extraBinDirs` | 21 | `~/.local/bin`, `/opt/homebrew/bin`, `/usr/local/bin` |
| **`FindExecutable(name)`** | 31 | Абсолютный путь утилиты (PATH + типичные папки) |
| `findExecutable` | 35 | То же с явным списком папок (для тестов) |
| **`WorkdirHeader`** | 51 | `X-Edy-Workdir` |
| **`CLIAdapter`** | 57 | Адаптер терминальных утилит в PTY |
| **`(*CLIAdapter).Stream`** | 59 | Запуск, чтение, очистка, ошибки |
| `lastRunes` | 164 | Последние N рун с «…» |
| `expandCommand` | 171 | Подстановка `{model}`/`{prompt}` |
| `renderPrompt` | 189 | Диалог → один текст (также для web) |
| `estimateTokens` | 219 | ≈ руны/4 |

### `internal/omniroute/climcp.go`

| Объявление | Строка | Описание |
|---|---|---|
| **`MCPHeader`** | 17 | `X-Edy-MCP` |
| **`MCPServersHeader`** | 22 | `X-Edy-MCP-Servers` |
| **`MCPAttach`** | 25 | URL MCP Edy, токен, внешние серверы |
| `mcpFlags` | 33 | Тип функции «флаги MCP для утилиты» |
| `mcpCapable` | 38 | Утилиты с MCP из аргументов (`claude`) |
| **`SupportsMCP(command)`** | 43 | Умеет ли утилита команды подключать MCP Edy |
| `claudeMCP` | 55 | Конфиг-файл и флаги для Claude Code |
| `checkMCPURL` | 98 | Только локальный `http://…/mcp` |
| `(*Server).extraMCP` | 111 | Внешние серверы каталога по именам из заголовка |

### `internal/omniroute/cliimages.go`

| Объявление | Строка | Описание |
|---|---|---|
| `addDirFlag` | 14 | `claude` → `--add-dir` |
| `imageExt` | 16 | MIME → расширение (png, jpg, gif, webp) |
| `materializeImages` | 22 | Картинки → временные файлы + текстовые ссылки |

### `internal/omniroute/sanitize.go`

| Объявление | Строка | Описание |
|---|---|---|
| `ansiCSI`, `ansiOSC`, `ansiOther` | 13-15 | Регулярки ESC-последовательностей |
| **`TermSanitizer`** | 21 | Поток терминала → чистый текст построчно |
| **`(*TermSanitizer).Write`** | 27 | Очередной кусок → готовые строки |
| **`(*TermSanitizer).Pending`** | 70 | Незавершённая строка (без изменения состояния) |
| **`(*TermSanitizer).Flush`** | 75 | Остаток после завершения процесса |
| `stripANSI` / `escComplete` / `incompleteUTF8Suffix` / `isNoise` | 81 / 91 / 110 / 127 | Вспомогательные |

### `internal/omniroute/toolemu.go`

| Объявление | Строка | Описание |
|---|---|---|
| `toolEmuAdapter` | 17 | Обёртка эмуляции tools |
| `toolProtocol` | 21 | Текст инструкции для модели |
| `(*toolEmuAdapter).Stream` | 36 | Буферизация и разбор `<tool_call>` |
| `withEmulatedTools` | 68 | Переписывание запроса |
| `textMessage` | 105 | Сообщение со строковым content |
| `toolCallRe` | 110 | Регулярка блока |
| `parseToolCalls` | 115 | Текст → (остаток, tool_calls) |
| `newCallID` | 141 | `call_<16 hex>` |

### `internal/omniroute/caps.go`

| Объявление | Строка | Описание |
|---|---|---|
| **`CapMCP` … `CapLongContext`** | 12-19 | ID тегов |
| **`CapInfo`** | 23 | ID, название, подсказка |
| **`CapCatalog`** | 30 | Все теги в порядке показа |
| **`CapTitle(id)`** | 42 | Человекочитаемое название (или сам id) |
| **`DefaultCaps(p)`** | 54 | Теги по умолчанию по kind/утилите |
| **`Caps(p)`** | 81 | Действующие теги (фильтр MCP, дублей, неизвестных) |
| **`HasCap(p, c)`** | 100 | Есть ли тег |

### `internal/omniroute/presets.go`

| Объявление | Строка | Описание |
|---|---|---|
| **`CLIPreset`** | 4 | Title, Command, Install, Note |
| **`CLIPresets`** | 17 | 13 терминальных агентов |
| **`APIPreset`** | 98 | Title, Kind, BaseURL, KeyURL, Note |
| **`APIPresets`** | 107 | 20 API-провайдеров |

### `internal/omniroute/embeddings.go`

| Объявление | Строка | Описание |
|---|---|---|
| `(*Server).handleEmbeddings` | 18 | `/v1/embeddings` для openai-провайдеров |

Детали: тело проксируется с заменённым `model`; ключи — как в чате, но собственный цикл
(`embeddings.go:72-117`): 429 → перерыв и следующий ключ; 401/403 (ID>0) → `DisableKey` и
следующий; другой не-2xx → `502` с текстом провайдера; 2xx → `usage.prompt_tokens` в учёт
(`output = 0`) и ответ как есть. Все ключи не сработали → `429 rate_limit_error` с последним
статусом. Ошибки `DisableKey`/`RecordUsage` здесь игнорируются. Используется поиском по смыслу
(`internal/semantic`, клиент `embeddings`).

### `internal/omniroute/models.go`

| Объявление | Строка | Описание |
|---|---|---|
| **`ListModels(ctx, p, key)`** | 21 | Список моделей провайдера (openai/anthropic), отсортирован |
| `listOpenAIModels` | 39 | `GET <base>/models`, срез префикса `models/` (Google) |
| `listAnthropicModels` | 76 | SDK `Models.ListAutoPaging` (limit 1000) |

API интерфейса вызывает его с **первым** включённым ключом и таймаутом 30 с
(`internal/api/api.go:211-222`).

### `internal/omniroute/web.go`

| Объявление | Строка | Описание |
|---|---|---|
| **`WebAsker`** | 13 | Что нужно адаптеру от браузера: `Ask(ctx, Conn, prompt, onText)` |
| **`WebAdapter`** | 19 | Адаптер веб-чатов (`Browser WebAsker`) |
| **`(*WebAdapter).Stream`** | 23 | Промпт → Ask → дельты |

### `internal/webchat/manager.go`

| Объявление | Строка | Описание |
|---|---|---|
| **`ErrLoginRequired`** | 28 | На странице нет поля ввода — нужен вход |
| **`ErrLoginOpen`** | 30 | Открыто окно входа, профиль занят |
| **`Conn`** | 34 | Профиль (имя провайдера), сайт, вид браузера |
| **`StateUnknown` … `StateError`** | 42-47 | Состояния подключения |
| **`Status`** | 51 | Состояние + сообщение + время |
| **`Manager`** | 63 | Браузеры, окна входа, статусы, лимиты по профилям |
| `browser`, `profileLimit` | 82 / 89 | Управляемый браузер; мьютекс/время/счётчик профиля |
| **`NewManager(dataDir)`** | 96 | Конструктор (`Log = io.Discard`) |
| **`(*Manager).Sites`** | 108 | Актуальный список сайтов (перечитывает файл) |
| **`(*Manager).BrowserInstalled(kind)`** | 123 | Доступен ли браузер |
| **`(*Manager).EnsureBrowser(ctx, kind)`** | 129 | Путь к браузеру, скачивание Chromium при необходимости |
| **`(*Manager).Status(profile)`** | 142 | Последнее состояние |
| `setStatus`, `profileDir`, `validProfile`, `baseArgs` | 154 / 160 / 165 / 172 | Вспомогательные |
| **`(*Manager).Login(ctx, c)`** | 187 | Окно входа без автоматизации + автопроверка после закрытия |
| **`(*Manager).Check(ctx, c)`** | 237 | Проверка входа (поле ввода на месте) |
| `record` | 273 | Ошибка → статус |
| `ensure`, `launch`, `singlePage`, `waitDevToolsURL` | 288 / 320 / 370 / 416 | Запуск и подключение к управляемому браузеру |
| `alive`, `stop`, `killGroup`, `workTab` | 440 / 451 / 463 / 473 | Жизненный цикл браузера |
| **`(*Manager).RemoveProfile(name)`** | 478 | Закрыть браузер, удалить профиль (cookies) |
| **`(*Manager).Close()`** | 497 | Закрыть все управляемые браузеры |
| `lockProfile`, `acquire` | 507 / 529 | Занятие профиля; вежливость (пауза, лимит) |
| `runFunc`, `withTab` | 554 / 557 | Вкладка и запуск действий с таймаутом |
| `pageState` | 581 | count/text/busy со страницы |
| `pageJS`, `stateJS`, `inputJS`, `focusJS`, `insertFallbackJS`, `genericSend`, `clickSendJS`, `dumpJS` | 589-670 | Скрипты на странице |
| **`(*Manager).Ask(ctx, c, prompt, onText)`** | 674 | Отправить промпт и стримить ответ |
| `ask` | 694 | Сама процедура (вставка, отправка, опрос) |
| `fail` | 824 | Скриншот + дамп в `web-debug` |

### `internal/webchat/chrome.go`

| Объявление | Строка | Описание |
|---|---|---|
| **`BrowserChromium`**, **`BrowserChrome`** | 26 / 29 | Виды браузера |
| `chromiumSnapshots` | 33 | Базовый URL snapshot-сборок |
| **`FindBrowser(dataDir, kind)`** | 37 | Путь к исполняемому файлу (`EDY_CHROME` приоритетнее) |
| `snapshotPlatform` | 70 | Платформа и имя архива |
| **`DownloadChromium(ctx, dataDir, log)`** | 83 | Скачать и распаковать свежий Chromium |
| `downloadChromium`, `download`, `unzip`, `readAll`, `extractFile` | 87 / 131 / 150 / 194 / 203 | Вспомогательные (zip slip защищён, симлинки сохраняются) |

### `internal/webchat/sites.go`

| Объявление | Строка | Описание |
|---|---|---|
| **`Site`** | 15 | Селекторы и параметры вежливости сайта |
| `(Site).minInterval` | 33 | Пауза как `time.Duration` |
| **`DefaultSites`** | 43 | chatgpt, claude, gemini |
| **`SitesFile`** | 77 | `web-sites.json` |
| **`LoadSites(dataDir)`** | 81 | Встроенные + переопределения, проверка обязательных полей |
| `merge` | 109 | Перенос непустых полей |

### `internal/vault/vault.go`

| Объявление | Строка | Описание |
|---|---|---|
| `keyringService`, `keyringUser`, `keySize` | 18-20 | `dev.edy.ide`, `master-key`, 32 |
| **`Vault`** | 24 | AEAD на мастер-ключе |
| **`New(masterKey)`** | 29 | Vault из 32-байтного ключа |
| **`FromKeychain()`** | 46 | Ключ из Keychain (создаёт при первом запуске) |
| **`(*Vault).Seal(plaintext)`** | 68 | Шифрование: `nonce || ct` |
| **`(*Vault).Open(sealed)`** | 77 | Расшифровка с проверкой целостности |

---

## 13. Тесты

### 13.1. Как запускать

```bash
go test ./internal/omniroute/            # ~6 с, без сети
go test ./internal/vault/                # мгновенно, Keychain замокан
go test ./internal/webchat/              # ~2 мин, нужен Chrome/Chromium; иначе браузерные тесты пропускаются
go test ./internal/omniroute/ -run TestCLI -v
EDY_CHROME=/path/to/chrome go test ./internal/webchat/ -run TestAsk -v
go test -race ./internal/omniroute/ ./internal/webchat/
```

CLI-тесты используют `/bin/sh`-скрипт и PTY — работают на macOS и Linux, не на Windows.
Браузерные тесты ищут Chrome в `$EDY_CHROME`, `/opt/pw-browsers/chromium-1194/chrome-linux/chrome`,
`/usr/bin/chromium`, `/usr/bin/google-chrome` (`webchat_test.go:41-53`) и запускают его headless с
`--no-sandbox --disable-gpu`. На коммите `b88fcbf` все три пакета проходят.

### 13.2. Фейки

- `newTestStore` (`server_test.go:22`) — настоящая SQLite во временной папке с vault на ключе из
  девяток.
- `fakeOpenAI` (`server_test.go:34`) — httptest-сервер: ключ `sk-limited` → 429 с
  `Retry-After: 30`, `sk-revoked` → 401, иначе SSE «При»+«вет» с usage 1M/0.5M; для
  `/embeddings` — фиксированный вектор и usage 1M. Запоминает увиденные ключи и последнее тело.
- Фейковый Anthropic — `httptest` со списком SSE-событий (в тестах `TestAnthropic*`).
- `fakeCLI` (`cli_test.go:20-30`) — sh-скрипт: падает, если stdout не терминал (проверка PTY);
  `fail*` → exit 3; `sleep*` → печатает вопрос и спит 30 с; иначе рисует спиннер/прогресс,
  печатает цветной ответ и `cwd`.
- `fakeBrowser` (`cli_test.go:179`) и `promptSpy` (`toolemu_test.go:86`) — реализации `WebAsker`.
- `fakeChat` и `chatVariants` (`webchat_test.go:23,319`) — HTML-страницы чатов (по буквам с
  кнопкой «Стоп»; Enter в contenteditable; только кнопка с aria-label; «застрявший» чат).
- `keyring.MockInit()` — мок Keychain в `vault_test.go`.

### 13.3. Что покрыто

| Тест | Файл:строка | Что проверяет |
|---|---|---|
| `TestRejectsMissingToken` | `server_test.go:109` | Без токена — 401 |
| `TestModelsList` | `server_test.go:119` | `/v1/models` отдаёт `oa/gpt-x` |
| `TestKeyRotationOn429NonStreaming` | `server_test.go:127` | 429 → следующий ключ; ключ на перерыве пропускается во втором запросе; поля (temperature) пробрасываются, model без префикса; расход $4 за 2 запроса |
| `TestRevokedKeyIsDisabled` | `server_test.go:166` | 401 → ключ отключён в БД, запрос успешен на втором |
| `TestAllKeysLimited` | `server_test.go:179` | Единственный ключ 429 → 429; повтор — «на перерыве» |
| `TestStreamingSSE` | `server_test.go:191` | Content-Type SSE, склейка текста, чанк с usage, `[DONE]`, model = `oa/gpt-x` |
| `TestDailyBudget` | `server_test.go:227` | Лимит $3: третий запрос — 429 «бюджет» |
| `TestBadModel` | `server_test.go:241` | Модель без `/` → 400, неизвестный провайдер → 404 |
| `TestAnthropicTranslationAndStream` | `server_test.go:259` | x-api-key, max_tokens по умолчанию, нет temperature, system, картинка base64, tool_use/tool_result, input_schema с additionalProperties; обратный перевод текста, tool_calls и usage |
| `TestAnthropicRotationOn429` | `server_test.go:360` | Ротация на 429 для Anthropic через SDK, стрим |
| `TestMediaNeedsCaps` | `server_test.go:393` | Картинка без `vision` → 400, с тегом → 200; звук без `audio` → 400 |
| `TestEmbeddingsEndpoint` | `server_test.go:410` | Ротация, model без префикса, расход $1, CLI → 400 |
| `TestCLIAdapterThroughServer` | `cli_test.go:46` | PTY (скрипт видит терминал), очистка спиннера/цветов, `X-Edy-Workdir`, оценка токенов |
| `TestCLIAdapterFailure` | `cli_test.go:67` | Ненулевой код → 502 с выводом |
| `TestCLIAdapterRejectsImages` | `cli_test.go:75` | Картинка неизвестной утилите (нет `vision`) → 400 |
| `TestCLIAdapterKilledOnCancel` | `cli_test.go:86` | Отмена убивает процесс быстро, вопрос утилиты попадает в ошибку |
| `TestRenderPrompt` | `cli_test.go:106` | Формат свёрнутого диалога |
| `TestAntigravityPresetKeepsModelWithSpaces` | `cli_test.go:128` | Модель с пробелами — один аргумент |
| `TestFindExecutableInExtraDirs` | `cli_test.go:139` | Поиск в доп. папках, неисполняемые файлы не находятся, PATH даёт абсолютный путь |
| `TestPresetsHaveInstallAndPrompt` | `cli_test.go:157` | У CLI-пресетов есть установка и `{prompt}` |
| `TestAPIPresetsAreConsistent` | `cli_test.go:168` | kind известен, у openai есть http-адрес |
| `TestWebAdapterThroughServer` | `cli_test.go:195` | Web-адаптер: Conn из провайдера, дельты «вет», finish stop |
| `TestMCPAttachRules` | `climcp_test.go:11` | SupportsMCP только `claude`; checkMCPURL (localhost да, внешние/не `/mcp` нет); конфиг `0600` с токеном удаляется |
| `TestMaterializeImages` | `climcp_test.go:45` | data: → файл, ссылка → текст, звук → ошибка, без картинок папки нет, уборка |
| `TestListOpenAIModels` / `TestListAnthropicModels` | `models_test.go:13` / `31` | Список и сортировка, срез `models/`, ошибка авторизации, CLI → ошибка |
| `TestTermSanitizer` | `sanitize_test.go:5` | Цвета, прогресс-бар, спиннер, OSC, разрыв ESC и UTF-8 между чтениями, backspace, хвост |
| `TestParseToolCalls` | `toolemu_test.go:13` | Два вызова (один в ```json), неизвестный остаётся текстом, уникальные id |
| `TestWithEmulatedToolsRendersHistory` | `toolemu_test.go:30` | system-протокол, история вызовов и результатов в текст |
| `TestToolEmulationThroughServer` | `toolemu_test.go:58` | Web-провайдер + tools → `tool_calls`, content «Ок.», описание в промпте |
| `TestSealOpenRoundTrip` / `TestOpenRejectsTampering` / `TestFromKeychainIsStable` | `vault_test.go:10/31/40` | Шифрование, целостность, один ключ между запусками |
| `TestAskStreamsAnswer` | `webchat_test.go:91` | Потоковый ответ из браузера |
| `TestPolitenessAndWatchdog` | `webchat_test.go:109` | Перезапуск упавшего браузера, пауза ≥1 с, дневной лимит 3 |
| `TestLoginRequiredSavesSnapshot` | `webchat_test.go:140` | Нет поля → `ErrLoginRequired` + диагностика |
| `TestLoadSitesOverrides` | `webchat_test.go:155` | Слияние с web-sites.json, новый сайт, ошибка неполного |
| `TestDownloadChromiumUnpacksWithSymlinks` | `webchat_test.go:174` | Скачивание LAST_CHANGE + архива, права, симлинк |
| `TestLoginWindowThenAutoCheck` | `webchat_test.go:223` | Статусы unknown → login_open; Ask → ErrLoginOpen; после закрытия окна → ok |
| `TestCheckReportsLoginRequired` | `webchat_test.go:256` | Check → login_required |
| `TestAskWithImages` | `webchat_test.go:446` | Картинка через `input[type=file]`, сгенерированная картинка → data URL |
| `TestAskPastesImagesWithoutFileInput` | `webchat_test.go:480` | Нет поля файла → вставка через paste, пустой промпт |
| `TestWebAdapterPassesImages` | `cli_test.go:225` | `image_url` → файл + подпись в промпте, картинка ответа в SSE |
| `TestChromeKindNeedsInstalledChrome` | `webchat_test.go:266` | Нет Chromium / неизвестный вид → ошибка |
| `TestUnzipRejectsPathTraversal` | `webchat_test.go:276` | zip slip отклоняется |
| `TestProfilesAreIsolated` | `webchat_test.go:289` | Разные процессы и папки профилей; RemoveProfile; `../evil` отклоняется |
| `TestSendStrategiesAndSingleTab` | `webchat_test.go:359` | Запасные селекторы, Enter в contenteditable, кнопка по aria-label, ровно одна вкладка |
| `TestStuckSendSavesDiagnostics` | `webchat_test.go:394` | Неотправляемое сообщение → ошибка и JSON-диагностика |

Не покрыто тестами: стрим с ошибкой в середине (`fail` после начала SSE), `X-Edy-MCP` через
сервер целиком (только юнит `claudeMCP`), `X-Edy-MCP-Servers`/`extraMCP`, внешние MCP в конфиге,
`--add-dir`, `Retry-After` в виде даты, ротация по 403, эмуляция tools со `stream: true`,
CLI с MCP-флагами, `RemoveProfile` во время открытого окна входа, реальные сайты.

---

## 14. Ограничения, непроверенное, грабли, идеи

### 14.1. Не проверено на реальных сервисах

- Встроенные селекторы `chatgpt`/`claude`/`gemini` — прямо помечены как непроверенные
  (`sites.go:35-41`). Ожидайте, что их придётся править через `web-sites.json` по файлам из
  `web-debug/`.
- Вход в Google (Gemini) в snapshot-сборке Chromium может блокироваться («браузер небезопасен»):
  у snapshot-сборок нет ключей Google API. Для Gemini надёжнее вид `chrome`. Не проверялось.
- Флаги неинтерактивного режима CLI-пресетов (`presets.go`) сверены с документацией, но не
  запускались в CI; особенно чувствительны `agy`, `copilot`, `cursor`/`agent`, `amp`.
- MCP-флаги Claude Code (`--mcp-config`, `--strict-mcp-config`, `--allowedTools`,
  `--disallowedTools`) — проверено только формирование аргументов, не реальный запуск.
- Совместимость API-пресетов со `stream_options.include_usage`, tools, картинками.

### 14.2. Грабли и вероятные баги

1. **Общий перерыв для всех «бесключевых» провайдеров.** Фиктивный ключ имеет `ID = -1`
   (`server.go:261`, `embeddings.go:64`), а `cooldowns` — общая карта по ID ключа. Если один
   локальный провайдер (например, vLLM) ответит 429, на перерыв уйдут **все** провайдеры без
   ключей (Ollama, LM Studio и т.д.) — им будет отвечаться «все ключи … на перерыве». Исправление:
   ключ перерыва `(providerID, keyID)`.
2. **Потеря строки в `TermSanitizer` при разрыве `\r\n` между чтениями.** PTY превращает `\n` в
   `\r\n`. Если чтение (4 КиБ) закончится на `\r`, а следующее начнётся с `\n`, то `\r` очистит
   текущую строку (`sanitize.go:51-52`), и строка пропадёт из ответа. Исправление: откладывать
   завершающий `\r` в `pending`, как незавершённые ESC.
3. **Нет учёта расходов без `usage`.** OpenAI-совместимые провайдеры, не возвращающие `usage` в
   стриме, не попадают ни в учёт, ни в бюджет (`server.go:204`). Бюджет проверяется только
   **до** запроса — один длинный ответ может сильно превысить лимит.
4. **Полночь бюджета — по локальному времени процесса**, а `RecordUsage` пишет `time.Now()`,
   игнорируя `Server.now` (в тестах с подменённым временем это расходится).
5. **Ошибка ключа провайдера возвращается клиенту тем же кодом 401/403**, что и неверный токен
   OmniRoute (`responder.fail` берёт `ue.Status`). Клиент может решить, что у него неверный токен
   Edy. Отличить можно только по `type` (`upstream_error` vs `authentication_error`).
6. **Эмуляция tools убивает стрим**: клиент получает весь ответ одним куском в конце
   (`toolemu.go:41-44`); для долгих CLI это выглядит как «зависание».
7. **Внутренние клиенты не стримят вообще**: `agent.InProcess` пишет в `httptest.ResponseRecorder`
   и отдаёт ответ после окончания `ServeHTTP` (`internal/agent/llm.go:176-180`).
8. **Тег `tools` не влияет на эмуляцию.** Решение «эмулировать или нет» зависит только от kind.
   OpenAI-совместимая модель без function calling (многие локальные) получит `tools` и ответит
   ошибкой. Идея: эмулировать, если `!HasCap(p, CapTools)` для любого kind.
9. **Теги на уровне провайдера, а не модели**: у OpenRouter одна модель видит картинки, другая
   нет, а тег один на всех.
10. **Промпт CLI в argv**: ограничение `ARG_MAX` (длинная история → «argument list too long») и
    видимость промпта в `ps`. Идея: поддержать `{prompt_file}` или подачу через stdin для
    утилит, которые это умеют.
11. **Нет таймаута и ввода у CLI**: интерактивный вопрос утилиты держит процесс до отмены
    контекста клиента; если у клиента нет таймаута — вечно.
12. **`X-Edy-Workdir` не проверяется**: любой обладатель токена может запустить утилиту в любой
    папке (а Aider/Codex там правят файлы и коммитят). Токен один на прокси, API и MCP.
13. **Окружение Edy наследуется CLI** (`cli.go:99`): например, `ANTHROPIC_API_KEY` в окружении
    переключит Claude Code с подписки на платный ключ (о чём предупреждает пресет).
14. **Временные файлы при аварийном завершении**: `edy-mcp-*.json` (с токеном, `0600`) и
    `edy-images-*` удаляются `defer`'ом — при `kill -9` Edy остаются в `$TMPDIR`.
15. **`SupportsMCP` по basename**: обёртка/алиас утилиты под другим именем теряет MCP и тег `mcp`.
16. **Ошибки внутри SSE OpenAI-провайдера теряются**: событие `data: {"error":…}` разбирается как
    пустой чанк (`openai.go:77-95`), клиент получает обрезанный ответ с успехом.
17. **Нет таймаутов HTTP**: `http.DefaultClient` в `OpenAIAdapter`, `handleEmbeddings`,
    `listOpenAIModels`, `download` (Chromium). Защита только через контекст.
18. **`WebAdapter` перестаёт отдавать текст, если сайт переписал начало ответа**
    (`web.go:43-45`): `sent` не обновляется, и все последующие версии тоже не начинаются с
    `sent` — остаток ответа теряется молча. Идея: в конце сравнить итог с `sent` и при
    расхождении отдать полный текст/ошибку.
19. **Глобальный мьютекс `Manager.mu` удерживается долго**: `ensure` вызывает `alive()` (до 5 с)
    и `launch` (до 30 с ожидания порта) под `m.mu`; `Login`/`RemoveProfile`/`Close` вызывают
    `stop()` (до ~5,3 с) под ним же. В это время блокируются `Status()` всех профилей — UI,
    опрашивающий статусы, «замирает».
20. **Гонка `RemoveProfile` и окна входа**: `RemoveProfile` убивает процесс окна входа, горутина
    из `Login` (`manager.go:223-231`) после `Wait()` вызывает `Check`, который через `launch`
    снова создаёт папку профиля (`MkdirAll`) и запускает браузер — уже после удаления
    провайдера. Также `RemoveProfile` не удаляет запись `m.logins` (это делает горутина) и
    `m.limits`.
21. **Дневной лимит считает неудачные запросы** и сбрасывается при перезапуске; `Check` не
    учитывается в лимите и паузе, хотя открывает сайт.
22. **Ответ веб-чата — `innerText`, а не markdown**: блоки кода и списки теряют разметку, что
    плохо для эмуляции tools (JSON внутри `<tool_call>` в отрисованном markdown обычно выживает,
    но гарантии нет — сайт может отрисовать `<tool_call>` как HTML-тег и съесть его).
23. **Скриншоты диагностики с перепиской** копятся в `web-debug/` без ротации.
24. **Нет проверки целостности скачанного Chromium** (только HTTPS); скачивается последняя
    snapshot-сборка (нестабильная dev-ветка), а не стабильный релиз.
25. **`EDY_CHROME` подменяет оба вида браузера** — если переменная случайно выставлена в
    окружении пользователя, «установленный Chrome» и «изолированный Chromium» станут одним.
26. **Vault без AAD**: шифротекст ключа одной строки можно скопировать в другую строку БД, и он
    расшифруется. Для локальной БД риск низкий, но привязка `AAD = provider_id/key_id` дешёвая.
27. **Одна нерасшифровываемая запись ломает всех**: `store.Keys` возвращает ошибку целиком, а не
    пропускает битый ключ.
28. **`AddProvider` не сохраняет `Caps`**, `scanProvider` молча глотает ошибку JSON в `caps`
    (получится пустой набор тегов, а не автоопределение).
29. **Отключённый ключ нельзя включить** — нет `EnableKey`; только удалить и добавить заново.
30. **`/v1/models` не знает о моделях CLI/web без списка** — если `Models` пуст, провайдер
    невидим для внешних программ, хотя запросы к нему работают.
31. **`max_tokens` по умолчанию 32000 для Anthropic** — для старых моделей с меньшим лимитом
    вывода это даст 400; клиент должен передавать `max_tokens` явно.
32. **`system` в середине диалога для Anthropic поднимается наверх**, порядок инструкций
    меняется; для CLI/web — остаётся на месте как «### Инструкции».
33. **`deleteKey` в API удаляет ключ по id без проверки, что он принадлежит провайдеру из пути**
    (`internal/api/api.go:523-535`) — подробнее в [05-api.md](05-api.md).

### 14.3. Идеи на будущее (TODO)

- Перерывы ключей по `(provider, key)`, персистентность перерывов и дневных счётчиков веб-чатов.
- Эмуляция tools по тегу `tools`, а не по kind; потоковая эмуляция (отдавать текст до первого
  `<tool_call>`, остальное — после разбора).
- Таймаут CLI-процесса (настройка провайдера), подача промпта через файл/stdin.
- MCP для других утилит (`codex`, `gemini`, `agy` и др.), если у них появятся флаги конфигурации
  из аргументов, — добавить в `mcpCapable`.
- Prompt caching и extended thinking для Anthropic; учёт стоимости кэша отдельно.
- Теги и цены на уровне модели (`провайдер/модель`), а не провайдера.
- Повтор запроса целиком при ошибке в середине стрима для внутренних клиентов (решение из
  `ARCHITECTURE.md`: «ошибка в середине стрима — повтор запроса целиком» — в роутере не
  реализовано, клиент получает ошибку).
- Включение ключа обратно (`EnableKey`), предупреждение в UI об отключённых ключах.
- Ротация/очистка `web-debug/`, кнопка «удалить диагностику».
- Веб-чаты: продолжение чата вместо нового на каждый запрос (меньше
  дублирования истории и меньше нагрузки на сайт).
- **Никогда**: подмена отпечатков, stealth-плагины, обход капчи, автоматический ввод пароля —
  запрещено владельцем проекта (см. 9.1).
