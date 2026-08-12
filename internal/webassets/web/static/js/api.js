// Small fetch wrapper: JSON in/out, and attaches the CSRF token (held in
// sessionStorage after login/me) to every state-changing request per
// ARCHITECTURE.md §1.4.

const CSRF_KEY = "watchparty_csrf_token";

export function setCSRFToken(token) {
  sessionStorage.setItem(CSRF_KEY, token);
}

export function getCSRFToken() {
  return sessionStorage.getItem(CSRF_KEY) || "";
}

export function clearCSRFToken() {
  sessionStorage.removeItem(CSRF_KEY);
}

/**
 * @param {string} path
 * @param {{method?: string, body?: any}} [opts]
 */
export async function api(path, opts = {}) {
  const method = opts.method || "GET";
  const headers = { "Content-Type": "application/json" };
  if (method !== "GET") {
    headers["X-CSRF-Token"] = getCSRFToken();
  }
  const resp = await fetch(path, {
    method,
    headers,
    credentials: "same-origin",
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
  });
  if (resp.status === 204) return null;
  let data = null;
  try {
    data = await resp.json();
  } catch {
    // no body
  }
  if (!resp.ok) {
    const err = new Error((data && data.message) || `request failed: ${resp.status}`);
    err.status = resp.status;
    err.code = data && data.error;
    throw err;
  }
  return data;
}
