package passwd

import (
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	const password = "correct horse 电池 staple"
	hash, err := Hash(password, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256$1000$") || strings.Contains(hash, password) {
		t.Fatalf("hash = %q", hash)
	}
	if !Verify(hash, password) {
		t.Fatal("the right password was rejected")
	}
	for _, wrong := range []string{"", "correct horse", password + " ", strings.ToUpper(password)} {
		if Verify(hash, wrong) {
			t.Fatalf("wrong password %q was accepted", wrong)
		}
	}

	// 每次的盐不同，所以同一个密码的哈希也不同。
	again, _ := Hash(password, 1000)
	if again == hash {
		t.Fatal("two hashes of the same password are identical (no salt?)")
	}
	if !Verify(again, password) {
		t.Fatal("second hash does not verify")
	}

	// 迭代次数记在哈希里：以后调大默认值，旧的哈希照样能核对。
	slow, _ := Hash(password, 5000)
	if !strings.HasPrefix(slow, "pbkdf2-sha256$5000$") || !Verify(slow, password) {
		t.Fatalf("hash with another iteration count: %q", slow)
	}
	def, _ := Hash(password, 0)
	if !strings.HasPrefix(def, "pbkdf2-sha256$600000$") {
		t.Fatalf("default iteration count: %q", def)
	}

	// 坏掉的数据一律核对不过，也不会把程序拖死或者弄崩。
	for _, bad := range []string{
		"", "plain", "pbkdf2-sha256$1000$onlythree", "md5$1000$c2FsdA$a2V5",
		"pbkdf2-sha256$abc$c2FsdA$a2V5", "pbkdf2-sha256$0$c2FsdA$a2V5", "pbkdf2-sha256$99999999999$c2FsdA$a2V5",
		"pbkdf2-sha256$1000$!!!$a2V5", "pbkdf2-sha256$1000$c2FsdA$!!!", "pbkdf2-sha256$1000$c2FsdA$",
	} {
		if Verify(bad, password) || Verify(bad, "") {
			t.Fatalf("malformed hash %q verified", bad)
		}
	}
}

func TestCheck(t *testing.T) {
	for _, ok := range []string{"12345678", "八个字符的密码呀", strings.Repeat("x", 128), strings.Repeat("长", 128)} {
		if err := Check(ok); err != nil {
			t.Errorf("Check(%q) = %v", ok, err)
		}
	}
	for _, weak := range []string{"", "1234567", "七个字符的密码", strings.Repeat("x", 129)} {
		if err := Check(weak); err == nil {
			t.Errorf("Check(%q) accepted", weak)
		}
	}
}

func TestGenerate(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		p, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		if len(p) < 16 || Check(p) != nil {
			t.Fatalf("generated password %q is too short", p)
		}
		// 不用容易看混的字符。
		if strings.ContainsAny(p, "0O1lIio") {
			t.Fatalf("generated password %q uses look-alike characters", p)
		}
		for _, r := range p {
			if r != '-' && !strings.ContainsRune(alphabet, r) {
				t.Fatalf("generated password %q has an unexpected character %q", p, r)
			}
		}
		if seen[p] {
			t.Fatalf("generated the same password twice: %q", p)
		}
		seen[p] = true
	}
}
