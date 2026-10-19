export const BASE = __ENV.BASE_URL || 'http://localhost:8080';
export const FAKE = __ENV.FAKE_URL || 'http://localhost:9090';
export const PARAMS = {
  headers: { 'Content-Type': 'application/json', Authorization: 'Bearer loadtest' },
};
export function chat(content, temperature = 0) {
  return JSON.stringify({
    model: 'gpt-4.1-mini',
    temperature,
    messages: [{ role: 'user', content }],
  });
}
