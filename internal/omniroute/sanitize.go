package omniroute

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	// CSI (цвета, перемещение курсора, очистка строки), OSC (заголовок окна,
	// гиперссылки) и прочие двухсимвольные ESC-последовательности.
	ansiCSI   = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	ansiOSC   = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	ansiOther = regexp.MustCompile(`\x1b[@-Z\\-_]`)
)

// TermSanitizer превращает сырой поток терминала в чистый текст. Работает
// построчно: строка отдаётся, только когда завершена переводом строки, потому что
// одиночный \r (прогресс-бары, спиннеры) перезаписывает её с начала.
type TermSanitizer struct {
	pending string // хвост предыдущего чтения: незавершённая ESC-последовательность или UTF-8 символ
	line    string // текущая строка, ещё не отданная наружу
}

// Write принимает очередной кусок вывода и возвращает готовый чистый текст.
func (s *TermSanitizer) Write(p []byte) string {
	data := s.pending + string(p)
	s.pending = ""

	// Незавершённую ESC-последовательность в конце оставляем до следующего чтения.
	if i := strings.LastIndexByte(data, 0x1b); i >= 0 && !escComplete(data[i:]) {
		data, s.pending = data[:i], data[i:]
	}
	// То же для разорванного многобайтового символа.
	if n := incompleteUTF8Suffix(data); n > 0 {
		s.pending = data[len(data)-n:] + s.pending
		data = data[:len(data)-n]
	}

	data = stripANSI(data)
	data = strings.ReplaceAll(data, "\r\n", "\n")

	var out strings.Builder
	for _, r := range data {
		switch {
		case r == '\n':
			out.WriteString(strings.TrimRight(s.line, " "))
			out.WriteByte('\n')
			s.line = ""
		case r == '\r':
			s.line = ""
		case r == '\b':
			if s.line != "" {
				_, size := utf8.DecodeLastRuneInString(s.line)
				s.line = s.line[:len(s.line)-size]
			}
		case r == '\t':
			s.line += "\t"
		case isNoise(r):
		default:
			s.line += string(r)
		}
	}
	return out.String()
}

// Pending возвращает незавершённую строку, не трогая её. Обычно это вопрос
// утилиты, который ждёт ответа: «Continue? (y/n)».
func (s *TermSanitizer) Pending() string {
	return strings.TrimRight(s.line, " ")
}

// Flush отдаёт остаток после завершения процесса.
func (s *TermSanitizer) Flush() string {
	rest := strings.TrimRight(s.line, " ")
	s.line, s.pending = "", ""
	return rest
}

func stripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	s = ansiOSC.ReplaceAllString(s, "")
	s = ansiCSI.ReplaceAllString(s, "")
	return ansiOther.ReplaceAllString(s, "")
}

// escComplete сообщает, закончилась ли ESC-последовательность в начале s.
func escComplete(s string) bool {
	if len(s) < 2 {
		return false
	}
	switch s[1] {
	case '[':
		for i := 2; i < len(s); i++ {
			if s[i] >= '@' && s[i] <= '~' {
				return true
			}
		}
		return false
	case ']':
		return strings.ContainsRune(s, 0x07) || strings.Contains(s[2:], "\x1b\\")
	default:
		return true
	}
}

func incompleteUTF8Suffix(s string) int {
	for n := 1; n <= 3 && n <= len(s); n++ {
		b := s[len(s)-n]
		if b < 0x80 {
			return 0
		}
		if utf8.RuneStart(b) {
			if !utf8.FullRuneInString(s[len(s)-n:]) {
				return n
			}
			return 0
		}
	}
	return 0
}

// isNoise отбрасывает управляющие символы и кадры спиннеров (Брайль, «⠋⠙⠹…»).
func isNoise(r rune) bool {
	return (r >= 0x2800 && r <= 0x28ff) || r == utf8.RuneError || unicode.IsControl(r)
}
