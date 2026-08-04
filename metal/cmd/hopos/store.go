// De object-store-adapter: verbindt slots.ObjectStore (de store-ops van de
// hop-ABI) met hoplock/s3 — dezelfde bucket, credentials en signer als de
// committed state, er komt geen tweede S3-client bij. Hier wonen ook de
// per-op-timeouts: de servicer-context (evict) cancelt een hangende
// transfer, dit is het plafond voor een transfer die "gewoon traag" is.
package main

import (
	"context"
	"io"
	"time"

	"github.com/xinix00/hoplock/s3"

	"hop/pkg/config"
)

const (
	// storeXferTimeout begrenst een pull/push: zo lang als een groot object
	// over een traag lijntje redelijkerwijs duurt, en kort genoeg dat een
	// dood endpoint een servicer niet een uur gijzelt.
	storeXferTimeout = 10 * time.Minute
	// storeMetaTimeout begrenst list/drop: klein, vaste responsgrootte.
	storeMetaTimeout = time.Minute
	// storeListMax is de cap op één listing. De hop-ABI-respons kan toch
	// maar ~8KB namen dragen; ver daarboven afkappen is dan geen verlies,
	// maar een onbegrensde listing zou HOP's heap laten groeien met de
	// bucket-inhoud van een ander (een app kan zijn eigen map volproppen).
	storeListMax = 4096
)

// s3Store implementeert slots.ObjectStore op een hoplock/s3-Backend.
type s3Store struct{ b *s3.Backend }

// newS3Store bouwt de store uit de al-gevalideerde S3-clusterconfig.
func newS3Store(cfg *config.S3LockConfig) s3Store {
	return s3Store{b: &s3.Backend{
		Endpoint:        cfg.Endpoint,
		Bucket:          cfg.Bucket,
		Key:             "unused", // object-API krijgt de key per aanroep
		Region:          cfg.Region,
		AccessKeyID:     cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey,
		SessionToken:    cfg.SessionToken,
		UsePathStyle:    cfg.UsePathStyle,
	}}
}

func (s s3Store) Pull(ctx context.Context, key string, w io.Writer) (int64, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, storeXferTimeout)
	defer cancel()
	return s.b.GetObjectTo(ctx, key, w)
}

func (s s3Store) Push(ctx context.Context, key string, size int64, sha256Hex string, r io.Reader) error {
	ctx, cancel := context.WithTimeout(ctx, storeXferTimeout)
	defer cancel()
	return s.b.PutObjectFrom(ctx, key, r, size, sha256Hex, "")
}

func (s s3Store) List(ctx context.Context, prefix string) ([]string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, storeMetaTimeout)
	defer cancel()
	return s.b.ListObjects(ctx, prefix, storeListMax)
}

func (s s3Store) Drop(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, storeMetaTimeout)
	defer cancel()
	return s.b.DeleteObject(ctx, key)
}
