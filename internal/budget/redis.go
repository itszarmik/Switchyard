package budget

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyPrefix starts every key of the Redis store.
const keyPrefix = "sy:b:v1:"

// chargeScript is Admit and Delta: it checks every scope and charges all of
// them only if every blocking one has room, atomically, since Redis runs a
// script without interleaving other commands.
//
// KEYS[2i-1] is the spend counter of scope i and KEYS[2i] its soft-threshold
// flag. ARGV[1] is the amount to charge, a non-negative integer in micro-USD.
// For scope i, ARGV[4i-2 .. 4i+1] are its limit, its soft threshold (0 for
// none), 1 if it blocks or 0 if it only notifies, and the Unix time at which
// its keys expire.
//
// It returns {0, i, ...}, the blocking scopes without room, having charged
// nothing; or {1, c1, ..., cn}, where ci has bit 0 set if scope i is now over
// its limit and bit 1 set if this charge reached its soft threshold first.
//
// The amount is passed to INCRBY as the string it arrived as: a Lua number
// is converted back to text as a double, which may use an exponent (1e+15)
// that INCRBY rejects. Comparisons use Lua numbers
// (doubles), which are exact for every counter below 2^53 micro-USD, far
// above config.MaxBudgetLimitUSD.
const chargeScript = `
local amount = tonumber(ARGV[1])
local n = #KEYS / 2
local full = {0}
for i = 1, n do
  local a = 4 * i - 2
  if ARGV[a + 2] == '1' then
    local spent = tonumber(redis.call('GET', KEYS[2 * i - 1]) or '0')
    if spent + amount > tonumber(ARGV[a]) then
      full[#full + 1] = i
    end
  end
end
if #full > 1 then
  return full
end
local out = {1}
for i = 1, n do
  local a = 4 * i - 2
  local counter, flag, expires = KEYS[2 * i - 1], KEYS[2 * i], ARGV[a + 3]
  local spent = redis.call('INCRBY', counter, ARGV[1])
  redis.call('EXPIREAT', counter, expires)
  local code = 0
  if spent > tonumber(ARGV[a]) then
    code = code + 1
  end
  local soft = tonumber(ARGV[a + 1])
  if soft > 0 and spent >= soft and redis.call('SET', flag, '1', 'NX') then
    redis.call('EXPIREAT', flag, expires)
    code = code + 2
  end
  out[#out + 1] = code
end
return out
`

// Result codes of chargeScript.
const (
	codeOverLimit   = 1
	codeSoftCrossed = 2
)

// RedisStore is a Store in Redis, shared by every replica. Each counter is
// the key sy:b:v1:{scope name}:{window start as yyyymmdd}, holding
// micro-USD, and expires two days after its window ends. The soft-threshold
// flag of a window is the same key followed by ":soft". Admit and Delta are
// one Lua script over every scope; Settle is INCRBY in a MULTI/EXEC
// transaction.
//
// The script reads and writes several keys, so the client must be a single
// Redis server (or a failover pair), not a Redis Cluster.
type RedisStore struct {
	client redis.UniversalClient
	prefix string
	charge *redis.Script
}

// NewRedisStore returns a Store that keeps its counters in client, which
// should be the non-evicting state instance (DESIGN §11.5).
func NewRedisStore(client redis.UniversalClient) Store {
	return newRedisStore(client, keyPrefix)
}

// newRedisStore returns a RedisStore whose keys start with prefix, so tests
// sharing a server do not collide.
func newRedisStore(client redis.UniversalClient, prefix string) *RedisStore {
	return &RedisStore{client: client, prefix: prefix, charge: redis.NewScript(chargeScript)}
}

// key returns the counter key of s in the window starting at start.
func (r *RedisStore) key(s Scope, start time.Time) string {
	return r.prefix + s.Name + ":" + start.Format("20060102")
}

// Admit implements Store.
func (r *RedisStore) Admit(ctx context.Context, scopes []Scope, est int64, now time.Time) (Admission, error) {
	if len(scopes) == 0 {
		return Admission{}, nil
	}
	windows, err := checkArgs(scopes, est, now)
	if err != nil {
		return Admission{}, err
	}
	a := Admission{scopes: slices.Clone(scopes), windows: windows}
	if err := r.run(ctx, "admit", &a, est); err != nil {
		return Admission{}, err
	}
	return a, nil
}

// Delta implements Store.
func (r *RedisStore) Delta(ctx context.Context, a *Admission, extra int64, _ time.Time) error {
	if extra <= 0 || len(a.scopes) == 0 {
		return nil
	}
	return r.run(ctx, "delta", a, extra)
}

// run executes chargeScript for amount over a's scopes and windows and, on
// success, adds amount to a.charged and records what it found in a.
func (r *RedisStore) run(ctx context.Context, op string, a *Admission, amount int64) error {
	keys := make([]string, 0, 2*len(a.scopes))
	args := make([]any, 0, 1+4*len(a.scopes))
	args = append(args, strconv.FormatInt(amount, 10))
	for i, s := range a.scopes {
		k := r.key(s, a.windows[i])
		keys = append(keys, k, k+":soft")
		block := "0"
		if s.Block {
			block = "1"
		}
		args = append(args,
			strconv.FormatInt(s.LimitMicro, 10),
			strconv.FormatInt(s.SoftAtMicro, 10),
			block,
			strconv.FormatInt(expiry(s.Window, a.windows[i]).Unix(), 10),
		)
	}
	res, err := r.charge.Run(ctx, r.client, keys, args...).Int64Slice()
	if err != nil {
		return unavailable(ctx, op, err)
	}
	if len(res) == 0 || (res[0] == 1 && len(res) != 1+len(a.scopes)) {
		return unavailable(ctx, op, errors.New("unexpected script result"))
	}
	if res[0] == 0 {
		full := make([]int, 0, len(res)-1)
		for _, i := range res[1:] {
			if i < 1 || int(i) > len(a.scopes) {
				return unavailable(ctx, op, errors.New("unexpected script result"))
			}
			full = append(full, int(i)-1)
		}
		return exhausted(a.scopes, a.windows, full)
	}
	var exceeded, crossed []Scope
	for i, code := range res[1:] {
		s := a.scopes[i]
		if code&codeOverLimit != 0 && !s.Block {
			exceeded = append(exceeded, s)
		}
		if code&codeSoftCrossed != 0 {
			crossed = append(crossed, s)
		}
	}
	a.charged += amount
	a.Exceeded, a.SoftCrossed = exceeded, crossed
	return nil
}

// Settle implements Store.
func (r *RedisStore) Settle(ctx context.Context, a Admission, actual int64) error {
	if actual < 0 {
		return fmt.Errorf("budget: negative actual cost %d", actual)
	}
	diff := actual - a.charged
	if diff == 0 || len(a.scopes) == 0 {
		return nil
	}
	_, err := r.client.TxPipelined(ctx, func(p redis.Pipeliner) error {
		for i, s := range a.scopes {
			k := r.key(s, a.windows[i])
			p.IncrBy(ctx, k, diff)
			// A late settlement may recreate a counter that has expired;
			// give it the same expiry again.
			p.ExpireAt(ctx, k, expiry(s.Window, a.windows[i]))
		}
		return nil
	})
	if err != nil {
		return unavailable(ctx, "settle", err)
	}
	return nil
}

// Spent implements Store.
func (r *RedisStore) Spent(ctx context.Context, s Scope, now time.Time) (int64, error) {
	v, err := r.client.Get(ctx, r.key(s, windowStart(s.Window, now))).Int64()
	switch {
	case errors.Is(err, redis.Nil):
		return 0, nil
	case err != nil:
		return 0, unavailable(ctx, "spent", err)
	}
	return v, nil
}
