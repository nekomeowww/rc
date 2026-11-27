import { ConnectionConfig, createDefaultAdapterFactory } from '@alibaba-group/opensandbox'
import { Sandbox } from 'e2b'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import type { EmulatorInstance } from './emulator'

import { startEmulator } from './emulator'

describe('E2B envd SDK conformance & cross-protocol interop (over mTLS Gateway)', () => {
  let emulator: EmulatorInstance

  beforeAll(async () => {
    emulator = await startEmulator()
  })

  afterAll(async () => {
    await emulator?.stop()
  })

  function createSandbox(token = emulator.token) {
    return new Sandbox({
      sandboxId: 'demo-sandbox',
      envdVersion: '0.11.0',
      envdAccessToken: token,
      sandboxUrl: emulator.envdUrl,
      debug: true,
    })
  }

  it('rejects unauthenticated requests with 401 while allowing /health', async () => {
    const health = await fetch(`${emulator.envdUrl}/health`)
    expect(health.status).toBe(204)
    expect(health.headers.get('x-envd-version')).toBe('0.11.0')

    const unauth = await fetch(`${emulator.envdUrl}/files?path=missing.txt`)
    expect(unauth.status).toBe(401)
  })

  it('shares filesystem state seamlessly between OpenSandbox execd and E2B envd', async () => {
    const factory = createDefaultAdapterFactory()
    const execdStack = factory.createExecdStack({
      connectionConfig: new ConnectionConfig({ domain: new URL(emulator.execdUrl).host }),
      execdBaseUrl: emulator.execdUrl,
      endpointHeaders: { 'X-EXECD-ACCESS-TOKEN': emulator.token },
    })

    const sharedPath = `${emulator.workspaceDir}/shared-interop.txt`
    await execdStack.files.writeFiles([
      { path: sharedPath, data: 'written-by-execd', mode: 644 },
    ])

    const sandbox = createSandbox()
    expect(await sandbox.files.read(sharedPath)).toBe('written-by-execd')

    const e2bPath = `${emulator.workspaceDir}/from-e2b.txt`
    await sandbox.files.write([{ path: e2bPath, data: 'written-by-e2b' }])

    const entries = await sandbox.files.list(emulator.workspaceDir)
    expect(entries.some(e => e.name === 'from-e2b.txt')).toBe(true)
    expect(await execdStack.files.readFile(e2bPath)).toBe('written-by-e2b')
  })

  it('runs foreground and background commands with stdout/stderr separation', async () => {
    const sandbox = createSandbox()

    const res = await sandbox.commands.run('echo e2b-out; echo e2b-err >&2', {
      cwd: emulator.workspaceDir,
    })
    expect(res.exitCode).toBe(0)
    expect(res.stdout.trim()).toBe('e2b-out')
    expect(res.stderr.trim()).toBe('e2b-err')

    const bg = await sandbox.commands.run('sleep 30', { background: true })
    const procs = await sandbox.commands.list()
    expect(procs.some(p => p.pid === bg.pid)).toBe(true)
    await bg.kill()
  })

  it('supports interactive PTY create, resize, and sendInput', async () => {
    const sandbox = createSandbox()
    let ptyOut = ''

    const pty = await sandbox.pty.create({
      cols: 80,
      rows: 24,
      onData: (data) => {
        ptyOut += new TextDecoder().decode(data)
      },
    })
    await sandbox.pty.resize(pty.pid, { cols: 100, rows: 30 })
    await sandbox.pty.sendInput(pty.pid, new TextEncoder().encode('echo E2B_PTY_OK && exit\n'))

    for (let i = 0; i < 20 && !ptyOut.includes('E2B_PTY_OK'); i++) {
      await new Promise(resolve => setTimeout(resolve, 100))
    }
    expect(ptyOut).toContain('E2B_PTY_OK')
  })
})
