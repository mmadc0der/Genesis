/**
 * Resume an existing sdk-minimal session instead of creating a second one.
 *
 * The pinned JSON-RPC server handles session/prompt by calling
 * ctx.agents.create({ sessionId }). That create path calls
 * persistence.create and raises once session.v3.jsonl is already on disk.
 * Resume is ctx.agents.resume({ resumeSessionId }) / persistence.open.
 * Cordis patch rows cannot wrap that server method, so this plugin replaces
 * agents.create before the first prompt. If create or resume is missing, apply
 * throws and the process does not pretend the session was resumed.
 *
 * GENESIS_TRANSPORT_RESTART=1 is a Genesis process restart of that same
 * session after a DSH TRANSPORT failure. The SDK still sends session/prompt,
 * which would append a user message. This plugin resumes the log, drops that
 * prompt, and wakes the agent loop so the next model request is the existing
 * history. A placeholder step message is used only to pass the loop's empty
 * first-step check, and its user/message append is discarded.
 *
 * A log is session.jsonl or session.vN.jsonl under
 * $DSH_HOME/sessions/<project>/<encoded session id>/. The current pin writes
 * session.v3.jsonl. Encoding matches dsh-session-persistence-jsonl encodeSegment.
 */
import fs from 'node:fs'
import path from 'node:path'

export const name = 'genesis-session-resume'

export const inject = ['agents']

const SESSION_LOG_NAME = /^session(?:\.v[1-9][0-9]*)?\.jsonl$/
const RESTART_MESSAGE_ID = 'genesis-transport-restart'

export function encodeSegment(raw) {
  if (typeof raw !== 'string' || raw.length === 0) {
    throw new Error('cannot encode an empty path segment')
  }
  if (raw === '.') return '~002E'
  if (raw === '..') return '~002E~002E'
  let out = ''
  for (let i = 0; i < raw.length; i++) {
    const code = raw.charCodeAt(i)
    const ch = String.fromCharCode(code)
    if (ch !== '~' && /^[A-Za-z0-9._-]$/.test(ch)) out += ch
    else out += `~${code.toString(16).toUpperCase().padStart(4, '0')}`
  }
  return out
}

export function isSessionLogName(filename) {
  return SESSION_LOG_NAME.test(filename)
}

function directoryEntries(dir) {
  try {
    return fs.readdirSync(dir, { withFileTypes: true })
  } catch {
    return null
  }
}

export function hasSessionLog(dshHome, sessionId) {
  if (typeof dshHome !== 'string' || dshHome.length === 0) return false
  if (typeof sessionId !== 'string' || sessionId.length === 0) return false
  let segment
  try {
    segment = encodeSegment(sessionId)
  } catch {
    return false
  }
  const projects = directoryEntries(path.join(dshHome, 'sessions'))
  if (projects === null) return false
  for (const project of projects) {
    if (!project.isDirectory()) continue
    const entries = directoryEntries(path.join(dshHome, 'sessions', project.name, segment))
    if (entries === null) continue
    for (const entry of entries) {
      if (entry.isFile() && isSessionLogName(entry.name)) return true
    }
  }
  return false
}

export function resumeOptions(options) {
  const source = options && typeof options === 'object' ? options : {}
  return {
    resumeSessionId: source.sessionId,
    agentOptions: source.agentOptions,
    signal: source.signal,
    setup: source.setup,
    parentAgent: source.parentAgent,
  }
}

function restartStepMessage() {
  return {
    id: RESTART_MESSAGE_ID,
    role: 'user',
    content: [],
    source: { kind: 'plugin', plugin: 'genesis-session-resume' },
  }
}

function armTransportRestart(agent) {
  if (
    !agent ||
    typeof agent.followup !== 'function' ||
    typeof agent.wakeDriver !== 'function' ||
    typeof agent.preStep !== 'function' ||
    !agent.session ||
    typeof agent.session.append !== 'function'
  ) {
    throw new Error('genesis-session-resume: transport restart cannot drive the resumed agent')
  }
  if (agent.followup.genesisTransportRestart) return
  const originalFollowup = agent.followup
  const originalPreStep = agent.preStep
  const originalAppend = agent.session.append
  agent.session.append = function genesisTransportRestartAppend(type, data, intent) {
    if (type === 'user/message' && data && data.id === RESTART_MESSAGE_ID) {
      return { type, data, seq: 0 }
    }
    return originalAppend.call(this, type, data, intent)
  }
  agent.preStep = async function genesisTransportRestartPreStep(target, position) {
    const decision = await originalPreStep.call(this, target, position)
    if (!decision || decision.kind === 'reject') return decision
    const messages = decision.messages || []
    if (position && position.step === 1 && messages.length === 0) {
      return { ...decision, messages: [restartStepMessage()] }
    }
    return decision
  }
  let woke = false
  const wrapped = function genesisTransportRestartFollowup(message) {
    if (process.env.GENESIS_TRANSPORT_RESTART !== '1') {
      return originalFollowup.call(this, message)
    }
    if (woke) return
    woke = true
    agent.wakeDriver()
  }
  wrapped.genesisTransportRestart = true
  agent.followup = wrapped
}

function install(agents) {
  if (!agents || typeof agents.create !== 'function' || typeof agents.resume !== 'function') {
    throw new Error('genesis-session-resume: agents.create and agents.resume are required')
  }
  if (agents.create.genesisSessionResume) return
  const original = agents.create
  const wrapped = async function genesisSessionResumeCreate(options) {
    const sessionId = options && typeof options.sessionId === 'string' ? options.sessionId : ''
    const home = typeof process.env.DSH_HOME === 'string' ? process.env.DSH_HOME : ''
    if (process.env.GENESIS_TRANSPORT_RESTART === '1') {
      if (!sessionId || !hasSessionLog(home, sessionId)) {
        throw new Error('genesis-session-resume: transport restart requires a persisted session')
      }
      const handle = await agents.resume.call(this, resumeOptions(options))
      armTransportRestart(handle && handle.agent)
      return handle
    }
    if (sessionId && hasSessionLog(home, sessionId)) {
      return agents.resume.call(this, resumeOptions(options))
    }
    return original.call(this, options)
  }
  wrapped.genesisSessionResume = true
  agents.create = wrapped
}

export function apply(ctx) {
  if (ctx && ctx.agents && typeof ctx.agents.create === 'function' && typeof ctx.agents.resume === 'function') {
    install(ctx.agents)
    return
  }
  if (ctx && typeof ctx.inject === 'function') {
    ctx.inject(['agents'], (agentCtx) => {
      install(agentCtx && agentCtx.agents)
    })
    return
  }
  throw new Error('genesis-session-resume: cannot reach agents.create or agents.resume')
}
