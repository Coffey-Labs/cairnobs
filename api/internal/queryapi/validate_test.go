package queryapi

import "testing"

func TestValidateSelectOnly(t *testing.T) {
	cases := []struct {
		name    string
		sql     string
		wantErr bool
	}{
		{"plain select", "SELECT * FROM logs LIMIT 10", false},
		{"lowercase select", "select service, count(*) from logs group by service", false},
		{"trailing semicolon allowed", "SELECT 1;", false},
		{"trailing semicolon and whitespace allowed", "SELECT 1;  ", false},
		{"empty", "", true},
		{"whitespace only", "   ", true},
		{"only a semicolon", ";", true},
		{"multiple statements", "SELECT 1; SELECT 2", true},
		{"insert", "INSERT INTO logs VALUES (1)", true},
		{"delete", "DELETE FROM logs", true},
		{"drop", "DROP TABLE logs", true},
		{"select with drop keyword smuggled in", "SELECT * FROM logs WHERE message = 'DROP TABLE logs'", true},
		{"non-select start", "WITH x AS (SELECT 1) SELECT * FROM x", true},
		{"trailing garbage after semicolon", "SELECT 1; DROP TABLE logs", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSelectOnly(tc.sql)
			if tc.wantErr && err == nil {
				t.Errorf("validateSelectOnly(%q) = nil, want error", tc.sql)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateSelectOnly(%q) = %v, want nil", tc.sql, err)
			}
		})
	}
}
