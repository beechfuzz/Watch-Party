// Thin WebSocket wrapper: JSON envelope framing, exponential-backoff
// reconnect, and an app-level heartbeat so a dropped connection is
// detected even when nothing else is happening. No framework.

const PROTOCOL_VERSION = 1;
const HEARTBEAT_INTERVAL_MS = 15000;
const RECONNECT_BASE_MS = 500;
const RECONNECT_MAX_MS = 15000;

export class PartyConnection {
  /**
   * @param {string} partyId
   * @param {(env: {type:string, payload:any}) => void} onMessage
   * @param {(state: "connecting"|"open"|"closed") => void} [onStateChange]
   */
  constructor(partyId, onMessage, onStateChange) {
    this.partyId = partyId;
    this.onMessage = onMessage;
    this.onStateChange = onStateChange || (() => {});
    this.ws = null;
    this.heartbeatTimer = null;
    this.reconnectAttempt = 0;
    this.closedByUser = false;
    this._connect();
  }

  _connect() {
    this.onStateChange("connecting");
    const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
    this.ws = new WebSocket(`${proto}//${window.location.host}/ws/parties/${encodeURIComponent(this.partyId)}`);

    this.ws.addEventListener("open", () => {
      this.reconnectAttempt = 0;
      this.onStateChange("open");
      this._startHeartbeat();
    });

    this.ws.addEventListener("message", (evt) => {
      let env;
      try {
        env = JSON.parse(evt.data);
      } catch {
        return;
      }
      if (env.protocol_version !== PROTOCOL_VERSION) {
        console.warn("watch party: ignoring message with unexpected protocol_version", env.protocol_version);
        return;
      }
      this.onMessage(env);
    });

    this.ws.addEventListener("close", () => {
      this._stopHeartbeat();
      this.onStateChange("closed");
      if (!this.closedByUser) this._scheduleReconnect();
    });

    this.ws.addEventListener("error", () => {
      this.ws.close();
    });
  }

  _scheduleReconnect() {
    const delay = Math.min(RECONNECT_BASE_MS * 2 ** this.reconnectAttempt, RECONNECT_MAX_MS);
    this.reconnectAttempt++;
    setTimeout(() => {
      if (!this.closedByUser) this._connect();
    }, delay);
  }

  _startHeartbeat() {
    this._stopHeartbeat();
    this.heartbeatTimer = setInterval(() => this.send("ping"), HEARTBEAT_INTERVAL_MS);
  }

  _stopHeartbeat() {
    if (this.heartbeatTimer) clearInterval(this.heartbeatTimer);
    this.heartbeatTimer = null;
  }

  send(type, payload) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    this.ws.send(JSON.stringify({ protocol_version: PROTOCOL_VERSION, type, payload }));
  }

  close() {
    this.closedByUser = true;
    this._stopHeartbeat();
    if (this.ws) this.ws.close(1000, "leaving");
  }
}
