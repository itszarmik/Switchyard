// Package cachetest holds the semantic-cache test sets (PLAN P6): pairs of
// the same question worded differently, in paraphrase.jsonl, which must
// hit, and pairs of the same question about different entities or
// numbers, in nearmiss.jsonl, which must never hit.
//
// The offline test checks the cache's mechanics over the sets with a
// scripted embedder. The live harness (build tag semanticlive, `make
// semantic-eval`) runs them through a real embedding model, named by the
// config at SWITCHYARD_SEMANTIC_CONFIG, and reports the hit rates and the
// similarity distributions.
package cachetest
