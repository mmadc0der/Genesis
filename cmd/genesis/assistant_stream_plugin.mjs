/**
 * Forward Cordis `agent/assistant-stream` frames as JSON-RPC `on_chunk`
 * notifications on the stdout the SDK server already uses for `session.event`.
 *
 * AssistantStreamFrame has no sessionId. The published Python client only
 * delivers notifications whose payload sessionId belongs to the running
 * session, so this plugin copies `agent.session.id` when the frame itself
 * does not already carry one. Every other frame field is left unchanged.
 */
export const name = 'genesis-assistant-stream'

function sessionIdOf(agent) {
  const session = agent && agent.session
  if (!session) return ''
  const id = session.id
  if (typeof id === 'string') return id
  if (typeof id === 'number' && Number.isFinite(id)) return String(id)
  return ''
}

function writeFrame(frame, agent) {
  if (!frame || typeof frame !== 'object') return
  const params = { ...frame }
  if (typeof params.sessionId !== 'string' || params.sessionId.length === 0) {
    const sessionId = sessionIdOf(agent)
    if (sessionId) params.sessionId = sessionId
  }
  process.stdout.write(`${JSON.stringify({ jsonrpc: '2.0', method: 'on_chunk', params })}\n`)
}

export function apply(ctx) {
  if (!ctx || typeof ctx.on !== 'function') return
  ctx.on('agent/assistant-stream', (payload) => {
    try {
      const body = payload || {}
      writeFrame(body.frame, body.agent)
    } catch {
      // A tee must not fail the model turn.
    }
  })
}
