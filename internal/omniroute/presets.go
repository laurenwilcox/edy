package omniroute

// CLIPreset — готовый профиль терминального агента.
type CLIPreset struct {
	Title string
	// Command — аргументы запуска: {model} заменяется на имя модели, {prompt} — на текст запроса.
	Command []string
	// Install — команда установки для macOS (выполняется через bash после подтверждения).
	Install string
	// Note — особенности утилиты, которые стоит знать до подключения.
	Note string
}

// CLIPresets — профили терминальных агентов. Флаги неинтерактивного режима
// сверены с документацией утилит; если версия у пользователя другая,
// провайдера можно добавить со своей командой.
var CLIPresets = map[string]CLIPreset{
	"claude-code": {
		Title:   "Claude Code (Anthropic)",
		Command: []string{"claude", "-p", "--model", "{model}", "{prompt}"},
		Install: "curl -fsSL https://claude.ai/install.sh | bash",
		Note:    "Работает по подписке Pro/Max. Если в окружении задан ANTHROPIC_API_KEY, используется он, а не подписка.",
	},
	"antigravity": {
		Title:   "Google Antigravity",
		Command: []string{"agy", "-p", "{prompt}", "--model", "{model}"},
		Install: "curl -fsSL https://antigravity.google/cli/install.sh | bash",
		Note:    "Модели Gemini и Claude по подписке Google. Имена моделей — из `agy models`.",
	},
	"codex": {
		Title:   "OpenAI Codex CLI",
		Command: []string{"codex", "exec", "-m", "{model}", "{prompt}"},
		Install: "npm install -g @openai/codex",
		Note:    "Работает по подписке ChatGPT или с ключом API. Нужен Node.js 22+.",
	},
	"gemini": {
		Title:   "Gemini CLI (Google)",
		Command: []string{"gemini", "-m", "{model}", "{prompt}"},
		Install: "npm install -g @google/gemini-cli",
		Note:    "Для личных подписок Google заменён Antigravity CLI; с ключом API продолжает работать.",
	},
	"qwen-code": {
		Title:   "Qwen Code (Alibaba)",
		Command: []string{"qwen", "-m", "{model}", "-p", "{prompt}"},
		Install: "npm install -g @qwen-code/qwen-code",
	},
	"cursor": {
		Title:   "Cursor CLI",
		Command: []string{"agent", "-p", "--model", "{model}", "{prompt}"},
		Install: "curl https://cursor.com/install -fsS | bash",
		Note:    "Без флага --force только предлагает правки, файлы не меняет.",
	},
	"copilot": {
		Title:   "GitHub Copilot CLI",
		Command: []string{"copilot", "-p", "{prompt}", "--model", "{model}"},
		Install: "npm install -g @github/copilot",
		Note:    "Нужна подписка Copilot. Имена моделей — из `copilot help`.",
	},
	"opencode": {
		Title:   "OpenCode",
		Command: []string{"opencode", "run", "-m", "{model}", "{prompt}"},
		Install: "curl -fsSL https://opencode.ai/install | bash",
		Note:    "Модель указывается как провайдер/модель, например anthropic/claude-sonnet-4-6.",
	},
	"amp": {
		Title:   "Amp (Sourcegraph)",
		Command: []string{"amp", "-x", "{prompt}"},
		Install: "npm install -g @sourcegraph/amp",
		Note:    "Модель выбирает сам Amp, имя модели в Edy ни на что не влияет.",
	},
	"goose": {
		Title:   "Goose (Block)",
		Command: []string{"goose", "run", "-t", "{prompt}"},
		Install: "brew install block-goose-cli",
		Note:    "Модель задаётся в настройках Goose (goose configure).",
	},
	"aider": {
		Title:   "Aider",
		Command: []string{"aider", "--model", "{model}", "--yes-always", "--no-pretty", "--message", "{prompt}"},
		Install: "curl -LsSf https://aider.chat/install.sh | sh",
		Note:    "Сам правит файлы в рабочей папке и делает коммиты.",
	},
	"ollama": {
		Title:   "Ollama (локальные модели)",
		Command: []string{"ollama", "run", "{model}", "--nowordwrap", "{prompt}"},
		Install: "brew install ollama",
		Note:    "Для потоковых ответов лучше подключить Ollama как API: http://localhost:11434/v1.",
	},
	"llm": {
		Title:   "LLM (Simon Willison)",
		Command: []string{"llm", "-m", "{model}", "{prompt}"},
		Install: "brew install llm",
		Note:    "Универсальная утилита с плагинами для десятков провайдеров.",
	},
}

// APIPreset — известный API-провайдер.
type APIPreset struct {
	Title   string
	Kind    string // store.KindOpenAI или store.KindAnthropic
	BaseURL string
	KeyURL  string // где получить ключ; пусто для локальных серверов
	Note    string
}

// APIPresets — облачные и локальные API. Почти все совместимы с форматом OpenAI.
var APIPresets = map[string]APIPreset{
	"anthropic":  {Title: "Anthropic (Claude)", Kind: "anthropic", KeyURL: "https://console.anthropic.com/settings/keys"},
	"openai":     {Title: "OpenAI", Kind: "openai", BaseURL: "https://api.openai.com/v1", KeyURL: "https://platform.openai.com/api-keys"},
	"google":     {Title: "Google Gemini API", Kind: "openai", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", KeyURL: "https://aistudio.google.com/apikey"},
	"openrouter": {Title: "OpenRouter (сотни моделей)", Kind: "openai", BaseURL: "https://openrouter.ai/api/v1", KeyURL: "https://openrouter.ai/keys", Note: "Один ключ — модели всех крупных провайдеров, есть бесплатные."},
	"deepseek":   {Title: "DeepSeek", Kind: "openai", BaseURL: "https://api.deepseek.com/v1", KeyURL: "https://platform.deepseek.com/api_keys"},
	"mistral":    {Title: "Mistral AI", Kind: "openai", BaseURL: "https://api.mistral.ai/v1", KeyURL: "https://console.mistral.ai/api-keys"},
	"xai":        {Title: "xAI (Grok)", Kind: "openai", BaseURL: "https://api.x.ai/v1", KeyURL: "https://console.x.ai"},
	"groq":       {Title: "Groq (очень быстрый)", Kind: "openai", BaseURL: "https://api.groq.com/openai/v1", KeyURL: "https://console.groq.com/keys"},
	"cerebras":   {Title: "Cerebras", Kind: "openai", BaseURL: "https://api.cerebras.ai/v1", KeyURL: "https://cloud.cerebras.ai"},
	"together":   {Title: "Together AI", Kind: "openai", BaseURL: "https://api.together.xyz/v1", KeyURL: "https://api.together.ai/settings/api-keys"},
	"fireworks":  {Title: "Fireworks AI", Kind: "openai", BaseURL: "https://api.fireworks.ai/inference/v1", KeyURL: "https://fireworks.ai/account/api-keys"},
	"perplexity": {Title: "Perplexity", Kind: "openai", BaseURL: "https://api.perplexity.ai", KeyURL: "https://www.perplexity.ai/settings/api", Note: "Модели с поиском в интернете."},
	"moonshot":   {Title: "Moonshot (Kimi)", Kind: "openai", BaseURL: "https://api.moonshot.ai/v1", KeyURL: "https://platform.moonshot.ai/console/api-keys"},
	"zai":        {Title: "Z.ai (GLM)", Kind: "openai", BaseURL: "https://api.z.ai/api/paas/v4", KeyURL: "https://z.ai/manage-apikey/apikey-list"},
	"qwen":       {Title: "Alibaba Qwen (DashScope)", Kind: "openai", BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", KeyURL: "https://modelstudio.console.alibabacloud.com"},
	"cohere":     {Title: "Cohere", Kind: "openai", BaseURL: "https://api.cohere.ai/compatibility/v1", KeyURL: "https://dashboard.cohere.com/api-keys"},
	"ollama":     {Title: "Ollama (локально)", Kind: "openai", BaseURL: "http://localhost:11434/v1", Note: "Ключ не нужен."},
	"lmstudio":   {Title: "LM Studio (локально)", Kind: "openai", BaseURL: "http://localhost:1234/v1", Note: "Ключ не нужен. Включите сервер в LM Studio."},
	"vllm":       {Title: "vLLM (локально)", Kind: "openai", BaseURL: "http://localhost:8000/v1", Note: "Ключ не нужен, если сервер запущен без --api-key."},
	"llamacpp":   {Title: "llama.cpp server (локально)", Kind: "openai", BaseURL: "http://localhost:8080/v1", Note: "Ключ не нужен."},
}
