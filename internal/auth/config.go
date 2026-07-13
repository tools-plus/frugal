package auth

import (
	"encoding/json"
	"fmt"

	"github.com/example/awsobs/internal/config"
)

// HasSecretKey reports whether a secret encryption key is configured. Without
// one, credential fields can't be stored or read.
func (s *Store) HasSecretKey() bool { return s.cipher.Available() }

// secretPtrs returns pointers to every secret string field in a Runtime, so a
// single pass can encrypt or decrypt them all.
func secretPtrs(rt *config.Runtime) []*string {
	ptrs := []*string{&rt.AWS.SecretAccessKey, &rt.AWS.SessionToken, &rt.IngestToken, &rt.Kubernetes.BearerToken, &rt.Kubernetes.Kubeconfig}
	for i := range rt.Kubernetes.Clusters {
		ptrs = append(ptrs, &rt.Kubernetes.Clusters[i].BearerToken)
	}
	for i := range rt.Native.Valkey {
		ptrs = append(ptrs, &rt.Native.Valkey[i].Password)
	}
	for i := range rt.Native.OpenSearch {
		ptrs = append(ptrs, &rt.Native.OpenSearch[i].Password)
	}
	for i := range rt.Native.RabbitMQ {
		ptrs = append(ptrs, &rt.Native.RabbitMQ[i].Password)
	}
	return ptrs
}

// GetConfig loads the runtime config with secret fields decrypted. seeded is
// false when nothing has been stored yet. Secrets that can't be decrypted (no
// key) are blanked with a warning rather than failing the whole load.
func (s *Store) GetConfig() (rt config.Runtime, seeded bool, err error) {
	var doc string
	switch e := s.sql.QueryRow(`SELECT doc FROM config WHERE id = 1`).Scan(&doc); e {
	case nil:
	default:
		return config.Runtime{}, false, nil // no row yet
	}
	if err := json.Unmarshal([]byte(doc), &rt); err != nil {
		return config.Runtime{}, false, fmt.Errorf("parse stored config: %w", err)
	}
	for _, p := range secretPtrs(&rt) {
		v, e := s.cipher.Decrypt(*p)
		if e != nil {
			s.logger.Printf("auth: config: dropping an unreadable secret (%v)", e)
			*p = ""
			continue
		}
		*p = v
	}
	return rt, true, nil
}

// SaveConfig encrypts secret fields and persists the runtime config. Fails if a
// secret is set but no encryption key is configured.
func (s *Store) SaveConfig(rt config.Runtime) error {
	for _, p := range secretPtrs(&rt) {
		v, err := s.cipher.Encrypt(*p)
		if err != nil {
			return err
		}
		*p = v
	}
	doc, err := json.Marshal(rt)
	if err != nil {
		return err
	}
	_, err = s.sql.Exec(`INSERT INTO config(id, doc) VALUES(1, ?) ON CONFLICT(id) DO UPDATE SET doc = excluded.doc`, string(doc))
	return err
}
