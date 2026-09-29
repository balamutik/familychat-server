package auth

import "testing"

func TestPasswordHashVerifiesOnlyOriginal(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "correct horse battery staple" {
		t.Fatal("password stored in plaintext")
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("valid password rejected")
	}
	if VerifyPassword(hash, "incorrect horse battery staple") {
		t.Fatal("wrong password accepted")
	}
	if VerifyPassword("malformed", "correct horse battery staple") {
		t.Fatal("malformed hash accepted")
	}
}

func TestPasswordRejectsShortInput(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
}
