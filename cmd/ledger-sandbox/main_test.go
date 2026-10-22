package main

import "testing"

func TestLocalProofDSNRejectsProductionAndHostOverrides(t *testing.T) {
	for _, dsn := range []string{"postgres://127.0.0.1:5433/osai_proof_test?sslmode=disable", "postgresql://localhost/osai_proof_test", "postgres://[::1]/osai_proof_test"} {
		if !localProofDSN(dsn) {
			t.Errorf("local proof DSN rejected: %s", dsn)
		}
	}
	for _, dsn := range []string{"", "postgres://127.0.0.1/osai", "postgres://remote.example/osai_proof_test", "postgres://localhost/osai_proof_test?host=remote.example", "postgres://localhost/osai_proof_test?dbname=osai", "postgres://localhost/osai_proof_test?hostaddr=10.0.0.1", "postgres://localhost/osai_proof_test?service=production", "https://localhost/osai_proof_test"} {
		if localProofDSN(dsn) {
			t.Errorf("unsafe DSN accepted: %s", dsn)
		}
	}
}
