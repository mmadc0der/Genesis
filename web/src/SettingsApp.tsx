import { useEffect, useState, type FormEvent } from "react";

export interface ModelConfig {
  provider: string;
  model: string;
  reasoning_effort: string;
  context_window: number;
  max_retries: number;
  max_backoff_ms: number;
  initial_delay_ms: number;
  stream_idle_timeout_ms: number;
  api_key_configured: boolean;
  api_key_masked?: string;
}

interface FormState {
  provider: string;
  model: string;
  reasoning_effort: string;
  context_window: number;
  max_retries: number;
  max_backoff_ms: number;
  initial_delay_ms: number;
  stream_idle_timeout_ms: number;
  api_key: string;
}

const DEFAULT_SETTINGS: FormState = {
  provider: "deepseek-official",
  model: "deepseek-flash",
  reasoning_effort: "high",
  context_window: 1000000,
  max_retries: 10,
  max_backoff_ms: 60000,
  initial_delay_ms: 500,
  stream_idle_timeout_ms: 172800000,
  api_key: "",
};

export function SettingsApp() {
  const [currentConfig, setCurrentConfig] = useState<ModelConfig | null>(null);
  const [form, setForm] = useState<FormState>(DEFAULT_SETTINGS);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<{ text: string; type: "success" | "error" } | null>(null);

  const fetchConfig = async () => {
    try {
      setLoading(true);
      const res = await fetch("/api/settings/model");
      if (!res.ok) {
        throw new Error(`Server returned HTTP ${res.status}`);
      }
      const data: ModelConfig = await res.json();
      setCurrentConfig(data);
      setForm({
        provider: data.provider || "deepseek-official",
        model: data.model || "deepseek-flash",
        reasoning_effort: data.reasoning_effort || "high",
        context_window: data.context_window || 1000000,
        max_retries: data.max_retries ?? 10,
        max_backoff_ms: data.max_backoff_ms || 60000,
        initial_delay_ms: data.initial_delay_ms || 500,
        stream_idle_timeout_ms: data.stream_idle_timeout_ms || 172800000,
        api_key: "",
      });
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err);
      setMessage({ text: `Failed to load model config: ${msg}`, type: "error" });
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    fetchConfig();
  }, []);

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setSaving(true);
    setMessage(null);

    try {
      const payload: Record<string, unknown> = {
        provider: form.provider.trim(),
        model: form.model.trim(),
        reasoning_effort: form.reasoning_effort,
        context_window: Number(form.context_window),
        max_retries: Number(form.max_retries),
        max_backoff_ms: Number(form.max_backoff_ms),
        initial_delay_ms: Number(form.initial_delay_ms),
        stream_idle_timeout_ms: Number(form.stream_idle_timeout_ms),
      };

      if (form.api_key.trim()) {
        payload.api_key = form.api_key.trim();
      }

      const res = await fetch("/api/settings/model", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
      });

      if (!res.ok) {
        const errorText = await res.text();
        throw new Error(errorText || `HTTP ${res.status}`);
      }

      const updated: ModelConfig = await res.json();
      setCurrentConfig(updated);
      setForm((prev) => ({ ...prev, api_key: "" }));
      setMessage({ text: "Model configuration updated successfully.", type: "success" });
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err);
      setMessage({ text: `Failed to save configuration: ${msg}`, type: "error" });
    } finally {
      setSaving(false);
    }
  };

  const handleResetDefaults = () => {
    setForm((prev) => ({
      ...DEFAULT_SETTINGS,
      api_key: prev.api_key,
    }));
    setMessage(null);
  };

  return (
    <div className="settings-container">
      <header className="settings-mast">
        <div className="settings-brand">
          <span className="settings-nameplate">Genesis</span>
          <span className="settings-edition">Model Configuration</span>
        </div>
        <div className="settings-mast-status">
          {currentConfig?.api_key_configured ? (
            <span className="status-chip chip-active">
              <i></i> Key Active {currentConfig.api_key_masked ? `(${currentConfig.api_key_masked})` : ""}
            </span>
          ) : (
            <span className="status-chip chip-warning">
              <i></i> No API Key
            </span>
          )}
        </div>
      </header>

      <main className="settings-content">
        <div className="settings-card">
          <div className="settings-card-header">
            <h2>Model & Runtime Settings</h2>
            <p className="settings-card-sub">
              Manage the language model, reasoning parameters, context windows, and retry policies for agent execution.
            </p>
          </div>

          {loading ? (
            <div className="settings-loading">Loading configuration…</div>
          ) : (
            <form onSubmit={handleSubmit} className="settings-form">
              {message && (
                <div className={`settings-alert alert-${message.type}`}>
                  {message.text}
                </div>
              )}

              <section className="form-section">
                <h3 className="section-title">Model Identification</h3>
                <div className="form-row">
                  <div className="form-group flex-1">
                    <label htmlFor="provider">Provider</label>
                    <input
                      id="provider"
                      type="text"
                      value={form.provider}
                      onChange={(e) => setForm({ ...form, provider: e.target.value })}
                      required
                    />
                    <span className="field-hint">e.g. <code>deepseek-official</code></span>
                  </div>

                  <div className="form-group flex-2">
                    <label htmlFor="model">Model ID</label>
                    <input
                      id="model"
                      type="text"
                      value={form.model}
                      onChange={(e) => setForm({ ...form, model: e.target.value })}
                      required
                    />
                    <div className="field-presets">
                      <span>Presets:</span>
                      <button
                        type="button"
                        className="preset-btn"
                        onClick={() => setForm({ ...form, model: "deepseek-flash" })}
                      >
                        deepseek-flash
                      </button>
                      <button
                        type="button"
                        className="preset-btn"
                        onClick={() => setForm({ ...form, model: "deepseek-chat" })}
                      >
                        deepseek-chat
                      </button>
                      <button
                        type="button"
                        className="preset-btn"
                        onClick={() => setForm({ ...form, model: "deepseek-reasoner" })}
                      >
                        deepseek-reasoner
                      </button>
                    </div>
                  </div>
                </div>
              </section>

              <section className="form-section">
                <h3 className="section-title">Reasoning & Thinking Budget</h3>
                <div className="form-row">
                  <div className="form-group flex-1">
                    <label htmlFor="reasoning_effort">Default Reasoning Effort</label>
                    <select
                      id="reasoning_effort"
                      value={form.reasoning_effort}
                      onChange={(e) => setForm({ ...form, reasoning_effort: e.target.value })}
                    >
                      <option value="off">off — no chain-of-thought tokens</option>
                      <option value="low">low — brief deliberation</option>
                      <option value="high">high — standard in-depth reasoning (default)</option>
                      <option value="max">max — maximum thinking budget</option>
                    </select>
                    <span className="field-hint">
                      Applied to agents that do not declare an override in <code>agents.d</code>.
                    </span>
                  </div>
                </div>
              </section>

              <section className="form-section">
                <h3 className="section-title">Context & Timeouts</h3>
                <div className="form-row">
                  <div className="form-group flex-1">
                    <label htmlFor="context_window">Context Window (Tokens)</label>
                    <input
                      id="context_window"
                      type="number"
                      min="1000"
                      max="10000000"
                      value={form.context_window}
                      onChange={(e) => setForm({ ...form, context_window: Number(e.target.value) })}
                      required
                    />
                    <span className="field-hint">Harness limit (default: 1,000,000)</span>
                  </div>

                  <div className="form-group flex-1">
                    <label htmlFor="stream_idle_timeout_ms">Stream Idle Timeout (ms)</label>
                    <input
                      id="stream_idle_timeout_ms"
                      type="number"
                      min="1000"
                      value={form.stream_idle_timeout_ms}
                      onChange={(e) => setForm({ ...form, stream_idle_timeout_ms: Number(e.target.value) })}
                      required
                    />
                    <span className="field-hint">Timeout before connection abort</span>
                  </div>
                </div>
              </section>

              <section className="form-section">
                <h3 className="section-title">Retry & Backoff Policy</h3>
                <div className="form-row">
                  <div className="form-group flex-1">
                    <label htmlFor="max_retries">Max Retries</label>
                    <input
                      id="max_retries"
                      type="number"
                      min="0"
                      max="100"
                      value={form.max_retries}
                      onChange={(e) => setForm({ ...form, max_retries: Number(e.target.value) })}
                      required
                    />
                    <span className="field-hint">Per-turn retry attempts</span>
                  </div>

                  <div className="form-group flex-1">
                    <label htmlFor="initial_delay_ms">Initial Delay (ms)</label>
                    <input
                      id="initial_delay_ms"
                      type="number"
                      min="1"
                      value={form.initial_delay_ms}
                      onChange={(e) => setForm({ ...form, initial_delay_ms: Number(e.target.value) })}
                      required
                    />
                    <span className="field-hint">First exponential backoff pause</span>
                  </div>

                  <div className="form-group flex-1">
                    <label htmlFor="max_backoff_ms">Max Delay (ms)</label>
                    <input
                      id="max_backoff_ms"
                      type="number"
                      min="100"
                      value={form.max_backoff_ms}
                      onChange={(e) => setForm({ ...form, max_backoff_ms: Number(e.target.value) })}
                      required
                    />
                    <span className="field-hint">Maximum backoff ceiling</span>
                  </div>
                </div>
              </section>

              <section className="form-section">
                <h3 className="section-title">Authentication & Credentials</h3>
                <div className="form-row">
                  <div className="form-group flex-1">
                    <label htmlFor="api_key">
                      DEEPSEEK_API_KEY
                      {currentConfig?.api_key_configured && (
                        <span className="key-status-label">
                          (Current: {currentConfig.api_key_masked || "Configured"})
                        </span>
                      )}
                    </label>
                    <input
                      id="api_key"
                      type="password"
                      placeholder={currentConfig?.api_key_configured ? "Leave empty to keep existing key" : "Enter API key"}
                      value={form.api_key}
                      onChange={(e) => setForm({ ...form, api_key: e.target.value })}
                      autoComplete="off"
                    />
                    <span className="field-hint">
                      Injected securely into the runner environment. Never exposed in agent files.
                    </span>
                  </div>
                </div>
              </section>

              <div className="form-actions">
                <button
                  type="submit"
                  className="btn-primary"
                  disabled={saving}
                >
                  {saving ? "Saving Changes…" : "Save Configuration"}
                </button>
                <button
                  type="button"
                  className="btn-secondary"
                  disabled={saving}
                  onClick={handleResetDefaults}
                >
                  Reset Defaults
                </button>
              </div>
            </form>
          )}
        </div>
      </main>
    </div>
  );
}
