package omniroute

import "testing"

func TestTermSanitizer(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"цвета", []string{"\x1b[1;32mготово\x1b[0m\r\n"}, "готово\n"},
		{"прогресс-бар перезаписывается", []string{"загрузка 10%\rзагрузка 50%\rзагрузка 100%\r\x1b[Kответ\n"}, "ответ\n"},
		{"спиннер", []string{"⠋ \x1b[?25l⠙ \r\x1b[2KПривет\n"}, "Привет\n"},
		{"OSC-заголовок", []string{"\x1b]0;title\x07текст\n"}, "текст\n"},
		{"ESC разорван между чтениями", []string{"a\x1b[3", "1mb\n"}, "ab\n"},
		{"UTF-8 разорван между чтениями", []string{"\xd0", "\x9f\xd1\x80\xd0\xb8\n"}, "При\n"},
		{"backspace", []string{"abc\b\bd\n"}, "ad\n"},
		{"хвост без перевода строки", []string{"конец"}, "конец"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var s TermSanitizer
			var got string
			for _, c := range tc.chunks {
				got += s.Write([]byte(c))
			}
			got += s.Flush()
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
