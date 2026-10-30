package main

import "testing"

func TestAllowedVerifiedEmail(t *testing.T) {
	a := &Authenticator{cfg: &Config{AllowedUsers: []string{"admin@example.com"}}}
	for _, tc := range []struct {
		c    claims
		want bool
	}{
		{claims{Sub: "other", Email: "admin@example.com"}, false},
		{claims{Sub: "other", Email: "ADMIN@example.com", EmailVerified: true}, true},
		{claims{Sub: "other", PreferredUsername: "admin@example.com"}, false},
		{claims{Sub: "admin@example.com"}, true},
	} {
		if got := a.allowed(tc.c); got != tc.want {
			t.Errorf("allowed(%+v) = %v", tc.c, got)
		}
	}
}

func TestUserInfoSubject(t *testing.T) {
	if sameSubject("", "") || sameSubject("one", "two") || !sameSubject("one", "one") {
		t.Fatal("UserInfo must match the ID token subject")
	}
}
