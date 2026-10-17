package codeindex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goSrc = `package shop

// Cart — корзина покупателя.
type Cart struct {
	Items []Item
	Owner string
}

type Item struct{ SKU string; Price int }

type Store interface {
	Load(id string) (*Cart, error)
	Save(c *Cart) error
}

func (c *Cart) Total() int {
	sum := 0
	for _, it := range c.Items {
		sum += it.Price
	}
	return sum
}

func ApplyDiscount(total, percent int) int {
	return total - total*percent/100
}
`

const pySrc = `import os


class UserRepository(BaseRepo):
    def find_by_email(self, email, *, active=True):
        return self.db.query(email)

    async def delete(self, user_id):
        pass


def send_welcome_email(user):
    return f"Привет, {user.name}"
`

func project(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"shop/cart.go":       goSrc,
		"shop/broken.go":     "package shop\nfunc {",
		"users/repo.py":      pySrc,
		"README.md":          "# Магазин\nОписание скидок и корзины.\n",
		"node_modules/x.js":  "ignored()",
		".hidden/secret.txt": "ignored",
	}
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	return dir
}

func TestSkeleton(t *testing.T) {
	idx, err := Build(context.Background(), project(t))
	if err != nil {
		t.Fatal(err)
	}
	sk := idx.Skeleton(20000)
	for _, want := range []string{
		"shop/cart.go (",
		"type Cart struct { Items, Owner }",
		"type Store interface { Load, Save }",
		"func (*Cart) Total() int",
		"func ApplyDiscount(total, percent int) int",
		"shop/broken.go", "не разобран",
		"class UserRepository(BaseRepo)",
		"def find_by_email(self, email, active)",
		"async def delete(self, user_id)",
		"def send_welcome_email(user)",
		"README.md",
	} {
		if !strings.Contains(sk, want) {
			t.Errorf("в скелете нет %q:\n%s", want, sk)
		}
	}
	if strings.Contains(sk, "node_modules") || strings.Contains(sk, ".hidden") {
		t.Errorf("служебные папки не должны попадать в скелет:\n%s", sk)
	}
	if strings.Contains(sk, "sum += it.Price") {
		t.Error("в скелете не должно быть тел функций")
	}
	if short := idx.Skeleton(10); !strings.Contains(short, "скелет обрезан") {
		t.Errorf("при маленьком лимите скелет должен обрезаться: %q", short)
	}
}

func TestSearch(t *testing.T) {
	idx, _ := Build(context.Background(), project(t))
	cases := map[string]string{
		"скидка discount percent": "ApplyDiscount",
		"total price of cart":     "Cart.Total",
		"find user by email":      "UserRepository",
		"welcome email":           "send_welcome_email",
	}
	for q, want := range cases {
		hits := idx.Search(q, 3)
		if len(hits) == 0 || hits[0].Symbol != want {
			t.Errorf("%q: первым ожидался %s, получено %+v", q, want, hits)
		}
	}
	out := FormatHits(idx.Search("ApplyDiscount", 1), 50)
	if !strings.Contains(out, "shop/cart.go · ApplyDiscount") || !strings.Contains(out, "return total - total*percent/100") {
		t.Fatalf("формат результата:\n%s", out)
	}
	if idx.Search("zzzqqq", 3) != nil || !strings.Contains(FormatHits(nil, 5), "Ничего не найдено") {
		t.Fatal("пустой результат")
	}
}

func TestTokenize(t *testing.T) {
	got := strings.Join(tokenize("parseHTTPRequest user_name Корзина"), ",")
	for _, want := range []string{"parsehttprequest", "parse", "http", "request", "user_name", "user", "name", "корзина"} {
		if !strings.Contains(","+got+",", ","+want+",") {
			t.Errorf("нет %q в %q", want, got)
		}
	}
}

func TestPythonWithoutPython3(t *testing.T) {
	dir := project(t)
	t.Setenv("PATH", t.TempDir()) // python3 недоступен
	idx, err := Build(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	sk := idx.Skeleton(20000)
	if !strings.Contains(sk, "class UserRepository(BaseRepo)") || !strings.Contains(sk, "def send_welcome_email(user)") {
		t.Fatalf("запасной разбор Python не сработал:\n%s", sk)
	}
}
