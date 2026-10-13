export async function responseJson(response) {
  const raw = await response.text();
  let result;
  try { result = JSON.parse(raw); } catch {
    const error = new Error(response.ok ? 'The controller returned an invalid response. Try again.'
      : `Agent controller unavailable (HTTP ${response.status}). Try again when the connection recovers.`);
    error.transient = !response.ok;
    throw error;
  }
  if (!response.ok) {
    const error = new Error(result.error || `Request failed (HTTP ${response.status})`);
    error.transient = [502, 503, 504].includes(response.status);
    throw error;
  }
  return result;
}
