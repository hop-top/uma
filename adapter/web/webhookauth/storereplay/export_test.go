package storereplay

import (
	"context"
	"strings"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Dump reports the entries stored across every bucket, expired ones
// included, and the raw stored text, for tests that assert what is kept.
func Dump(ctx context.Context, s *Store) (entries int, raw string, err error) {
	var all strings.Builder
	err = s.st.View(ctx, s.ch.Tenant, func(r store.Reader) error {
		for n := range s.buckets {
			cur, err := r.Protocol(ctx, s.ch, s.bucketName(n))
			if model.IsCode(err, model.NotFound) {
				continue
			}
			if err != nil {
				return err
			}
			st, err := decode(cur.Value)
			if err != nil {
				return err
			}
			entries += len(st.Entries)
			all.Write(cur.Value.Data)
		}
		return nil
	})
	return entries, all.String(), err
}

// Corrupt overwrites the stored state of the first bucket with a value of
// the wrong shape.
func Corrupt(ctx context.Context, s *Store) error {
	return s.st.Transact(ctx, s.ch.Tenant, func(tx store.Tx) error {
		name := s.bucketName(0)
		cur, err := tx.Protocol(ctx, s.ch, name)
		if err != nil {
			return err
		}
		return tx.PutProtocol(ctx, store.ProtocolState{
			Channel: s.ch, Key: name, Revision: "corrupt",
			Value: model.Extension{Name: protocolName, Version: protocolVersion, Data: []byte(`[1,2,3]`)},
		}, cur.Revision)
	})
}

// Used is the number of buckets holding a record.
func Used(ctx context.Context, s *Store) (n int, err error) {
	err = s.st.View(ctx, s.ch.Tenant, func(r store.Reader) error {
		for i := range s.buckets {
			_, err := r.Protocol(ctx, s.ch, s.bucketName(i))
			if model.IsCode(err, model.NotFound) {
				continue
			}
			if err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}
