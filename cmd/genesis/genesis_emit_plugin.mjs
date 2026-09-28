/**
 * Ask the SDK to deliver one genesis.emit notification on the stdout it
 * already uses for session.event. The Python runner turns that into an
 * emit frame. Genesis stamps source and id. This file does not read a
 * listener address or GENESIS_SYNC_TOKEN.
 */
export const name = 'genesis-emit'

const TOOL_NAME = 'genesis_emit'

function writeEmit(input, sessionId) {
  const body = input && typeof input === 'object' ? input : {}
  const params = {
    sessionId: typeof sessionId === 'string' ? sessionId : '',
    type: body.type,
    subject: body.subject,
    data: body.data,
    source: body.source,
    id: body.id,
  }
  process.stdout.write(
    `${JSON.stringify({ jsonrpc: '2.0', method: 'genesis.emit', params })}\n`,
  )
}

function toolDefinition() {
  return {
    name: TOOL_NAME,
    description:
      'Emit one CloudEvent. Set type and subject. data is an optional JSON object.',
    parameters: {
      type: 'object',
      properties: {
        type: { type: 'string' },
        subject: { type: 'string' },
        data: { type: 'object' },
      },
      required: ['type', 'subject'],
    },
    execute(args, toolCtx) {
      const session = toolCtx && toolCtx.session
      const sessionId = session && (session.id || session.sessionId)
      writeEmit(args, typeof sessionId === 'string' ? sessionId : '')
      return { emitted: true }
    },
  }
}

function tryRegister(register, tool) {
  if (typeof register !== 'function') return false
  try {
    register(tool)
    return true
  } catch {
    return false
  }
}

export function apply(ctx) {
  try {
    if (!ctx || typeof ctx.on !== 'function') return
    const tool = toolDefinition()
    if (
      tryRegister(ctx.tool, tool) ||
      tryRegister(ctx.registerTool, tool) ||
      tryRegister(ctx.addTool, tool)
    ) {
      return
    }
    ctx.on('dsh/register-tools', (register) => {
      tryRegister(register, tool)
    })
  } catch {
    // A missing tool API must not fail the session.
  }
}
