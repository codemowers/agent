const count = value => typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : undefined;

export function turnMetrics(durationMs, usage = {}, apiDurationMs) {
  return Object.fromEntries(Object.entries({durationMs: count(durationMs), apiDurationMs: count(apiDurationMs),
    inputTokens: count(usage.input_tokens), outputTokens: count(usage.output_tokens),
    cacheReadTokens: count(usage.cache_read_input_tokens ?? usage.cached_input_tokens),
    cacheWriteTokens: count(usage.cache_creation_input_tokens)}).filter(([, value]) => value !== undefined));
}
