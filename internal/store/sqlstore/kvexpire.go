package sqlstore

import (
	"context"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

var _ core.KVExpirer = (*Store)(nil)

// KVDelete implements core.KVExpirer.
func (s *Store) KVDelete(ctx context.Context, key string) error {
	_, err := s.exec(ctx, s.conn(ctx), "DELETE FROM kv WHERE key=?", key)
	return err
}

// likeEscaper escapes the wildcards of a LIKE pattern. The escape character
// is the backslash.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// KVDeleteOlder implements core.KVExpirer.
func (s *Store) KVDeleteOlder(ctx context.Context, prefix string, before time.Time) (int, error) {
	res, err := s.exec(ctx, s.conn(ctx), `DELETE FROM kv WHERE key LIKE ? ESCAPE '\' AND updated_ns < ?`,
		likeEscaper.Replace(prefix)+"%", before.UnixNano())
	if err != nil {
		return 0, err
	}
	n, err := s.rowsAffected(res)
	return int(n), err
}
