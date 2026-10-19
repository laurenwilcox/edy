package omniroute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/laurenwilcox/name/internal/store"
)

// cliModelCommands — как узнать у утилиты список моделей: аргументы
// подкоманды; nil — у утилиты нет такой команды.
var cliModelCommands = map[string][]string{
	"codex":    {"debug", "models"}, // JSON-каталог {"models":[{"slug","visibility"}]}
	"agy":      {"models"},          // «id<TAB>название»
	"agent":    {"models"},          // Cursor: «  * id (default)» / «  - id»
	"opencode": {"models"},          // «провайдер/модель» построчно
}

// claudeAliases — Claude Code не умеет выдавать список, но понимает
// псевдонимы, которые сам разворачивает в последнюю версию модели.
var claudeAliases = []string{"opus", "sonnet", "haiku"}

// listCLIModels спрашивает список моделей у самой CLI-утилиты.
func listCLIModels(ctx context.Context, p store.Provider) ([]string, error) {
	if len(p.Command) == 0 {
		return nil, errors.New("у провайдера нет команды запуска")
	}
	tool := filepath.Base(p.Command[0])
	if tool == "claude" {
		return append([]string(nil), claudeAliases...), nil
	}
	sub, ok := cliModelCommands[tool]
	if !ok {
		return nil, fmt.Errorf("утилита %s не умеет выдавать список моделей — впишите их вручную", tool)
	}
	bin, err := FindExecutable(p.Command[0])
	if err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, bin, sub...).Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", tool, strings.Join(sub, " "), err)
	}
	var ids []string
	if tool == "codex" {
		ids, err = parseCodexModels(out)
	} else {
		ids = parseModelLines(string(out))
	}
	if err == nil && len(ids) == 0 {
		err = fmt.Errorf("%s не вернула ни одной модели", tool)
	}
	return ids, err
}

// parseCodexModels берёт из каталога codex модели, которые он сам
// показывает в списке (visibility "hide" — служебные и устаревшие).
func parseCodexModels(out []byte) ([]string, error) {
	var cat struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(out, &cat); err != nil {
		return nil, fmt.Errorf("не удалось разобрать каталог моделей codex: %w", err)
	}
	var ids []string
	for _, m := range cat.Models {
		if m.Slug != "" && m.Visibility != "hide" {
			ids = append(ids, m.Slug)
		}
	}
	return ids, nil
}

var modelIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

// parseModelLines достаёт имена моделей из текстового списка: первое слово
// строки (после маркеров «*» и «-»), похожее на идентификатор модели.
// Заголовки вроде «Available models:» отсеиваются: в имени модели всегда
// есть цифра, дефис, точка или «/».
func parseModelLines(out string) []string {
	var ids []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimLeft(strings.TrimSpace(line), "*- "))
		if len(f) == 0 {
			continue
		}
		id := f[0]
		if !modelIDRe.MatchString(id) || !strings.ContainsAny(id, "0123456789-./") || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}
