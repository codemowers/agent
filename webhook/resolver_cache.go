package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	resolverFreshFor = 10 * time.Second
	resolverStaleFor = 10 * time.Minute
)

type staleEntry[V any] struct {
	value   V
	fetched time.Time
}

// staleCache reuses a lookup for resolverFreshFor and, when the API server
// fails, keeps serving the last good value for resolverStaleFor.
type staleCache[V any] struct {
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]staleEntry[V]
}

func newStaleCache[V any](now func() time.Time) *staleCache[V] {
	return &staleCache[V]{now: now, entries: map[string]staleEntry[V]{}}
}

func (c *staleCache[V]) get(key string, fetch func() (V, error)) (V, error) {
	now := c.now()
	c.mu.Lock()
	entry, hit := c.entries[key]
	c.mu.Unlock()
	if hit && now.Sub(entry.fetched) < resolverFreshFor {
		return entry.value, nil
	}
	value, err := fetch()
	if errors.Is(err, errNotFound) {
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
		return value, err
	}
	if err != nil {
		if hit && now.Sub(entry.fetched) < resolverStaleFor {
			return entry.value, nil
		}
		return value, err
	}
	c.mu.Lock()
	c.entries[key] = staleEntry[V]{value: value, fetched: now}
	c.mu.Unlock()
	return value, nil
}

// cachedResolver keeps receivers working through brief API server outages.
// Deleted protocols, receivers, bindings and keys stop matching within
// resolverFreshFor once the API server reports them missing.
type cachedResolver struct {
	inner     receiverResolver
	tokens    *tokenReviewCache
	protocols *staleCache[receiverProtocol]
	receivers *staleCache[webhookReceiver]
	bindings  *staleCache[[]receiverBinding]
	keys      *staleCache[[]byte]
}

func newCachedResolver(inner receiverResolver) *cachedResolver {
	return &cachedResolver{inner: inner, tokens: newTokenReviewCache(inner.ReviewToken),
		protocols: newStaleCache[receiverProtocol](time.Now), receivers: newStaleCache[webhookReceiver](time.Now),
		bindings: newStaleCache[[]receiverBinding](time.Now), keys: newStaleCache[[]byte](time.Now)}
}

func (c *cachedResolver) Protocol(ctx context.Context, name string) (receiverProtocol, error) {
	return c.protocols.get(name, func() (receiverProtocol, error) { return c.inner.Protocol(ctx, name) })
}
func (c *cachedResolver) Receiver(ctx context.Context, namespace, name string) (webhookReceiver, error) {
	return c.receivers.get(namespace+"/"+name, func() (webhookReceiver, error) { return c.inner.Receiver(ctx, namespace, name) })
}
func (c *cachedResolver) Bindings(ctx context.Context, r webhookReceiver) ([]receiverBinding, error) {
	return c.bindings.get(r.kind()+"/"+r.Metadata.Namespace+"/"+r.Metadata.Name, func() ([]receiverBinding, error) { return c.inner.Bindings(ctx, r) })
}

// ReceiverKey is keyed by the receiver UID and its reference, so a recreated
// receiver or a changed reference fetches the Secret again.
func (c *cachedResolver) ReceiverKey(ctx context.Context, r webhookReceiver) ([]byte, error) {
	key := r.Metadata.Namespace + "/" + r.Metadata.UID
	if ref := r.Spec.Senders.SecretKeyRef; ref != nil {
		key += "/" + ref.Namespace + "/" + ref.Name + "/" + ref.Key
	}
	return c.keys.get(key, func() ([]byte, error) { return c.inner.ReceiverKey(ctx, r) })
}
func (c *cachedResolver) ReviewToken(ctx context.Context, token, audience string) (string, error) {
	return c.tokens.ReviewToken(ctx, token, audience)
}
