package main

import (
	"database/sql"
	"encoding/json"
)

func NewPostgresService(db *sql.DB) (*Service, error) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS customer_institutions(id text PRIMARY KEY, body jsonb NOT NULL);
 CREATE TABLE IF NOT EXISTS customer_credentials(id text PRIMARY KEY, public_id text NOT NULL UNIQUE, institution_id text NOT NULL REFERENCES customer_institutions(id), secret_hash text NOT NULL, body jsonb NOT NULL);
 CREATE TABLE IF NOT EXISTS customer_beneficiaries(id text PRIMARY KEY, institution_id text NOT NULL REFERENCES customer_institutions(id), body jsonb NOT NULL);
 CREATE TABLE IF NOT EXISTS customer_beneficiary_actions(id bigserial PRIMARY KEY, beneficiary_id text NOT NULL REFERENCES customer_beneficiaries(id), institution_id text NOT NULL REFERENCES customer_institutions(id), action text NOT NULL CHECK (action IN ('PROPOSE','APPROVE','REJECT','DISABLE')), actor_id text NOT NULL, reason text NOT NULL, occurred_at timestamptz NOT NULL DEFAULT clock_timestamp());
 ALTER TABLE customer_beneficiary_actions DROP CONSTRAINT IF EXISTS customer_beneficiary_actions_action_check;
 ALTER TABLE customer_beneficiary_actions ADD CONSTRAINT customer_beneficiary_actions_action_check CHECK (action IN ('PROPOSE','APPROVE','REJECT','DISABLE'));
 CREATE OR REPLACE FUNCTION reject_beneficiary_action_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'beneficiary audit is append only'; END $$;
 DROP TRIGGER IF EXISTS customer_beneficiary_actions_immutable ON customer_beneficiary_actions;
 CREATE TRIGGER customer_beneficiary_actions_immutable BEFORE UPDATE OR DELETE ON customer_beneficiary_actions FOR EACH ROW EXECUTE FUNCTION reject_beneficiary_action_mutation();`)
	if err != nil {
		return nil, err
	}
	s := NewService()
	s.db = db
	rows, err := db.Query(`SELECT body FROM customer_institutions`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw []byte
		var inst Institution
		if err = rows.Scan(&raw); err != nil {
			break
		}
		if err = json.Unmarshal(raw, &inst); err != nil {
			break
		}
		s.institutions[inst.ID] = &inst
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = db.Query(`SELECT body FROM customer_credentials`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw []byte
		var stored struct {
			Credential
			SecretHash string `json:"secret_hash"`
		}
		if err = rows.Scan(&raw); err != nil {
			break
		}
		if err = json.Unmarshal(raw, &stored); err != nil {
			break
		}
		cred := stored.Credential
		cred.SecretHash = stored.SecretHash
		s.credentials[cred.ID] = &cred
		s.publicToCred[cred.PublicID] = &cred
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = db.Query(`SELECT body FROM customer_beneficiaries`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw []byte
		var beneficiary Beneficiary
		if err = rows.Scan(&raw); err != nil {
			break
		}
		if err = json.Unmarshal(raw, &beneficiary); err != nil {
			break
		}
		s.beneficiaries[beneficiary.ID] = &beneficiary
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Service) saveInstitution(inst Institution) error {
	if s.db == nil {
		return nil
	}
	raw, err := json.Marshal(inst)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO customer_institutions(id,body) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET body=excluded.body`, inst.ID, string(raw))
	return err
}
func (s *Service) saveCredential(cred Credential) error {
	if s.db == nil {
		return nil
	}
	raw, err := json.Marshal(struct {
		Credential
		SecretHash string `json:"secret_hash"`
	}{cred, cred.SecretHash})
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO customer_credentials(id,public_id,institution_id,secret_hash,body) VALUES($1,$2,$3,$4,$5) ON CONFLICT(id) DO UPDATE SET secret_hash=excluded.secret_hash,body=excluded.body`, cred.ID, cred.PublicID, cred.InstitutionID, cred.SecretHash, string(raw))
	return err
}
func (s *Service) seedSandbox(inst Institution, cred Credential) error {
	if s.db == nil {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	instBody, _ := json.Marshal(inst)
	credBody, _ := json.Marshal(struct {
		Credential
		SecretHash string `json:"secret_hash"`
	}{cred, cred.SecretHash})
	if _, err = tx.Exec(`INSERT INTO customer_institutions(id,body) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET body=excluded.body`, inst.ID, string(instBody)); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO customer_credentials(id,public_id,institution_id,secret_hash,body) VALUES($1,$2,$3,$4,$5) ON CONFLICT(id) DO UPDATE SET secret_hash=excluded.secret_hash,body=excluded.body`, cred.ID, cred.PublicID, cred.InstitutionID, cred.SecretHash, string(credBody)); err != nil {
		return err
	}
	return tx.Commit()
}
