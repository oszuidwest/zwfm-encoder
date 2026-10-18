package util

import (
	"strings"
	"testing"
)

func TestPasswordHash(t *testing.T) {
	t.Parallel()
	password := " a long Unicode password 🔐 " //nolint:gosec // G101: fixed test credential, never used by the application.
	encoded, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if encoded == second || strings.Contains(encoded, password) {
		t.Fatal("hash must use a fresh salt and never contain plaintext")
	}
	if err := ValidatePasswordHash(encoded); err != nil {
		t.Fatal(err)
	}
	if !VerifyPasswordHash(password, encoded) || VerifyPasswordHash(strings.TrimSpace(password), encoded) {
		t.Fatal("password verification changed whitespace or accepted the wrong password")
	}
	for _, tc := range []struct{ name, hash string }{
		{"plaintext", "secret"},
		{"weak work factor", strings.Replace(encoded, "$600000$", "$1$", 1)},
		{"unbounded work factor", strings.Replace(encoded, "$600000$", "$9999999999999$", 1)},
		{"wrong algorithm", strings.Replace(encoded, "sha256", "sha1", 1)},
		{"truncated", encoded[:len(encoded)-1]},
		{"extra field", encoded + "$extra"},
		{"invalid base64", passwordHashPrefix + strings.Repeat("!", 22) + "$" + strings.Repeat("!", 43)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ValidatePasswordHash(tc.hash) == nil || VerifyPasswordHash(password, tc.hash) {
				t.Fatal("malformed or unsupported hash accepted")
			}
		})
	}
}

func TestPasswordHashIndependentVector(t *testing.T) {
	t.Parallel()
	// Generated independently with Python hashlib.pbkdf2_hmac, not HashPassword.
	const encoded = "$pbkdf2-sha256$600000$AAECAwQFBgcICQoLDA0ODw$Et87JbYU0ZmJdgknCpB5MNcY95zIh89x2laBohNEjGk" //nolint:gosec // G101: public test vector, not an application credential.
	if !VerifyPasswordHash("independent test password", encoded) {
		t.Fatal("PBKDF2 algorithm, salt, key length or work factor changed")
	}
}
