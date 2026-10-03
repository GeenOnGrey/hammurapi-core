package migrations

import (
	"context"
	"database/sql"

	"github.com/pressly/goose/v3"
)

// 00008 (PLT.HMR-0004, arch §10 "+7"): the chat history accumulated with the
// previous agent is not carried over to Pi. Messages are deleted in batches
// without one long transaction; attachment links are set to NULL by the
// existing foreign key, and the cleaner removes attachment files without
// references. Irreversible: Down restores nothing.
func init() {
	goose.AddNamedMigrationNoTxContext("00008_chat_reset.go", upChatReset, downChatReset)
}

// ChatResetBatch is the number of messages deleted per statement.
var ChatResetBatch = 10000

func upChatReset(ctx context.Context, db *sql.DB) error {
	for {
		res, err := db.ExecContext(ctx, `
			DELETE FROM chat_messages
			WHERE id IN (SELECT id FROM chat_messages ORDER BY created_at LIMIT $1)`, ChatResetBatch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
	}
}

func downChatReset(context.Context, *sql.DB) error { return nil }
