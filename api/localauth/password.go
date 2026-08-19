package localauth

import "golang.org/x/crypto/bcrypt"

// dummyPasswordHash is a precomputed bcrypt hash of an arbitrary,
// never-used-as-a-real-password string -- handleLogin runs
// ComparePassword against this on the "no such user" path purely to pay
// the same bcrypt cost the "wrong password" path already pays, closing
// a response-time side channel that would otherwise let a caller
// distinguish the two despite their identical error message. There is
// no real password behind this hash; it exists only to burn comparable
// CPU time.
const dummyPasswordHash = "$2a$10$fH9R3O6ViQ6c7bq0N7yyBO1JP2TOw/bZEopMyZKBYrBjgYBZO9rCa"

// HashPassword and ComparePassword are the only two places this package
// touches a raw password -- everywhere else, a user is identified by an
// already-issued session token (see token.go), never by re-checking a
// password on every request.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func ComparePassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
