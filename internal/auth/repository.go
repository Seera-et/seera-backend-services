package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository stores the local record of an authenticated account.
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// Account is the app_users row.
type Account struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	AvatarURL   string    `json:"avatar_url"`
	CreatedAt   time.Time `json:"created_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`

	// IsNew reports that this call created the account rather than finding it.
	//
	// There is no separate sign-up with Google — the first sign-in *is* the
	// sign-up — so this is the only moment the app can tell a new account from
	// a returning one. It is what a welcome state keys off, and what Phase 2
	// will use to decide whether to offer to adopt the transcripts sitting in
	// this browser's localStorage.
	IsNew bool `json:"is_new"`
}

// Ensure creates or refreshes the account behind a verified token.
//
// Called on sign-in and before anything that references a user by foreign key,
// rather than from the auth middleware: a write on every authenticated request
// would turn every corpus read into a database write for no gain.
//
// The claims are taken as authoritative on each call because they are: the
// token is signed by Supabase, and a user who changes their Google display name
// should not be stuck with the old one until they make a new account.
func (r *Repository) Ensure(ctx context.Context, user User) (Account, error) {
	// xmax is zero on a row produced by INSERT and non-zero on one produced by
	// the DO UPDATE branch, which is how a single upsert can also report which
	// of the two it did. The alternative — SELECT then INSERT — is two round
	// trips and races with a second concurrent sign-in.
	const query = `
		INSERT INTO app_users (id, email, display_name, avatar_url)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''))
		ON CONFLICT (id) DO UPDATE SET
			email        = EXCLUDED.email,
			display_name = COALESCE(EXCLUDED.display_name, app_users.display_name),
			avatar_url   = COALESCE(EXCLUDED.avatar_url, app_users.avatar_url),
			last_seen_at = now()
		RETURNING id, email, COALESCE(display_name, ''), COALESCE(avatar_url, ''),
		          created_at, last_seen_at, (xmax = 0) AS inserted`

	var account Account
	err := r.pool.QueryRow(ctx, query, user.ID, user.Email, user.DisplayName, user.AvatarURL).
		Scan(&account.ID, &account.Email, &account.DisplayName, &account.AvatarURL,
			&account.CreatedAt, &account.LastSeenAt, &account.IsNew)
	if err != nil {
		return Account{}, fmt.Errorf("upsert app_user: %w", err)
	}
	return account, nil
}

// Get reads an account without touching it. Returns pgx.ErrNoRows when the
// account has never been Ensure'd.
func (r *Repository) Get(ctx context.Context, id string) (Account, error) {
	const query = `
		SELECT id, email, COALESCE(display_name, ''), COALESCE(avatar_url, ''),
		       created_at, last_seen_at
		FROM app_users
		WHERE id = $1`

	var account Account
	err := r.pool.QueryRow(ctx, query, id).
		Scan(&account.ID, &account.Email, &account.DisplayName, &account.AvatarURL,
			&account.CreatedAt, &account.LastSeenAt)
	switch {
	case err == nil:
		return account, nil
	case err == pgx.ErrNoRows:
		return Account{}, err
	default:
		return Account{}, fmt.Errorf("get app_user: %w", err)
	}
}
