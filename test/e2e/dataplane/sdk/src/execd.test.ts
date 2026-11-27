import { Buffer } from 'node:buffer'
import { ConnectionConfig, createDefaultAdapterFactory } from '@alibaba-group/opensandbox'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import WebSocket from 'ws'

import type { EmulatorInstance } from './emulator'

import { startEmulator } from './emulator'

describe('OpenSandbox execd SDK conformance (over mTLS Gateway)', () => {
  let emulator: EmulatorInstance

  beforeAll(async () => {
    emulator = await startEmulator()
  })

  afterAll(async () => {
    await emulator?.stop()
  })

  function createStack(token = emulator.token) {
    const factory = createDefaultAdapterFactory()
    return factory.createExecdStack({
      connectionConfig: new ConnectionConfig({ domain: new URL(emulator.execdUrl).host }),
      execdBaseUrl: emulator.execdUrl,
      endpointHeaders: token ? { 'X-EXECD-ACCESS-TOKEN': token } : undefined,
    })
  }

  it('responds to /ping without authentication and rejects unauthenticated /command with 401', async () => {
    const stack = createStack('')
    expect(await stack.health.ping()).toBe(true)

    const res = await fetch(`${emulator.execdUrl}/command`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ command: 'echo blocked' }),
    })
    expect(res.status).toBe(401)
  })

  it('supports directory creation, file CRUD, search, replaceContents, and moveFiles', async () => {
    const stack = createStack()
    const projectDir = `${emulator.workspaceDir}/execd-project`

    await stack.files.createDirectories([{ path: projectDir, mode: 755 }])
    await stack.files.writeFiles([
      { path: `${projectDir}/hello.txt`, data: 'hello opensandbox\nsecond line\nhello again\n', mode: 644 },
      { path: `${projectDir}/extra.txt`, data: 'to be moved', mode: 644 },
    ])

    expect(await stack.files.readFile(`${projectDir}/hello.txt`)).toBe(
      'hello opensandbox\nsecond line\nhello again\n',
    )

    const infos = await stack.files.getFileInfo([`${projectDir}/hello.txt`])
    expect(infos[`${projectDir}/hello.txt`]?.mode).toBe(644)

    const matches = await stack.files.search({ path: projectDir, pattern: '*.txt' })
    expect(matches).toHaveLength(2)

    await stack.files.replaceContents([
      { path: `${projectDir}/hello.txt`, oldContent: 'hello', newContent: 'hi' },
    ])
    expect(await stack.files.readFile(`${projectDir}/hello.txt`)).toBe(
      'hi opensandbox\nsecond line\nhi again\n',
    )

    await stack.files.moveFiles([
      { src: `${projectDir}/extra.txt`, dest: `${projectDir}/moved.txt` },
    ])
    await stack.files.deleteFiles([`${projectDir}/moved.txt`])

    const remaining = await stack.files.search({ path: projectDir, pattern: '*.txt' })
    expect(remaining).toHaveLength(1)
  })

  it('streams foreground stdout and stderr over SSE', async () => {
    const stack = createStack()
    const stdout: string[] = []
    const stderr: string[] = []

    const result = await stack.commands.run(
      'echo "from-execd-stdout" && echo "from-execd-stderr" >&2',
      { workingDirectory: emulator.workspaceDir },
      {
        onStdout: (msg) => {
          stdout.push(msg.text)
        },
        onStderr: (msg) => {
          stderr.push(msg.text)
        },
      },
    )

    expect(result.error).toBeUndefined()
    expect(stdout.join('')).toContain('from-execd-stdout')
    expect(stderr.join('')).toContain('from-execd-stderr')
  })

  it('manages background commands with incremental cursor logs, status, and interrupt', async () => {
    const stack = createStack()
    const bg = await stack.commands.run('echo bg-line-1; echo bg-line-2; sleep 30', {
      background: true,
      workingDirectory: emulator.workspaceDir,
    })
    expect(bg.id).toBeTruthy()
    const commandId = bg.id!

    let logs = ''
    let cursor = 0
    for (let i = 0; i < 20; i++) {
      const res = await stack.commands.getBackgroundCommandLogs(commandId, cursor)
      logs += res.content
      cursor = res.cursor ?? cursor
      if (logs.includes('bg-line-1') && logs.includes('bg-line-2'))
        break
      await new Promise(resolve => setTimeout(resolve, 100))
    }

    expect(logs).toContain('bg-line-1')
    expect(logs).toContain('bg-line-2')

    const status = await stack.commands.getCommandStatus(commandId)
    expect(status.running).toBe(true)

    await stack.commands.interrupt(commandId)
  })

  it('handles interactive WebSocket /pty sessions with resize through the mTLS Gateway', async () => {
    const wsUrl = `${emulator.execdUrl.replace(/^http/, 'ws')}/pty?cols=80&rows=24`
    const output = await new Promise<string>((resolve, reject) => {
      const ws = new WebSocket(wsUrl, {
        headers: { 'X-EXECD-ACCESS-TOKEN': emulator.token },
      })
      let acc = ''
      const timer = setTimeout(() => {
        ws.close()
        reject(new Error(`WebSocket PTY timed out; output so far: ${acc}`))
      }, 5_000)

      ws.on('open', () => {
        ws.send(JSON.stringify({ type: 'resize', cols: 120, rows: 40 }))
        ws.send(Buffer.from('echo EXECD_WS_PTY_OK && exit\n'))
      })
      ws.on('message', (data) => {
        acc += data.toString()
        if (acc.includes('EXECD_WS_PTY_OK')) {
          clearTimeout(timer)
          ws.close()
          resolve(acc)
        }
      })
      ws.on('error', (err) => {
        clearTimeout(timer)
        reject(err)
      })
    })

    expect(output).toContain('EXECD_WS_PTY_OK')
  })
})
