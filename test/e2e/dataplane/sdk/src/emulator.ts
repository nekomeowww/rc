import type { ChildProcess } from 'node:child_process'

import { execFileSync, spawn } from 'node:child_process'
import { rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { createInterface } from 'node:readline'

export interface EmulatorReadyInfo {
  gatewayUrl: string
  execdUrl: string
  envdUrl: string
  token: string
  homeDir: string
  workspaceDir: string
}

export interface EmulatorInstance extends EmulatorReadyInfo {
  stop: () => Promise<void>
}

const REPO_ROOT = resolve(import.meta.dirname, '../../../../..')

/**
 * Compiles and launches the local Go Data Plane emulator (rckube.Supervisor +
 * mTLS execd/http+ws + mTLS envd/connectrpc + Data Plane Gateway).
 */
export async function startEmulator(): Promise<EmulatorInstance> {
  const binaryPath = join(tmpdir(), `rc-dataplane-emulator-${process.pid}-${Date.now()}`)
  execFileSync('go', ['build', '-o', binaryPath, './test/e2e/dataplane/emulator'], {
    cwd: REPO_ROOT,
    stdio: 'inherit',
  })

  const child: ChildProcess = spawn(binaryPath, [], {
    cwd: REPO_ROOT,
    stdio: ['ignore', 'pipe', 'inherit'],
  })

  const info = await new Promise<EmulatorReadyInfo>((resolveInfo, rejectInfo) => {
    const timer = setTimeout(() => {
      child.kill('SIGKILL')
      rejectInfo(new Error('timed out waiting for dataplane emulator startup JSON'))
    }, 15_000)

    child.once('error', (err) => {
      clearTimeout(timer)
      rejectInfo(err)
    })

    child.once('exit', (code) => {
      clearTimeout(timer)
      rejectInfo(new Error(`dataplane emulator exited early with code ${code ?? 'null'}`))
    })

    if (!child.stdout) {
      clearTimeout(timer)
      rejectInfo(new Error('dataplane emulator stdout is not available'))
      return
    }

    const rl = createInterface({ input: child.stdout })
    rl.once('line', (line) => {
      clearTimeout(timer)
      rl.close()
      try {
        resolveInfo(JSON.parse(line) as EmulatorReadyInfo)
      }
      catch (err) {
        rejectInfo(err)
      }
    })
  })

  return {
    ...info,
    stop: async () => {
      await new Promise<void>((done) => {
        if (child.exitCode !== null) {
          done()
          return
        }
        child.once('exit', () => done())
        child.kill('SIGTERM')
        setTimeout(() => {
          if (child.exitCode === null)
            child.kill('SIGKILL')
          done()
        }, 3_000)
      })
      rmSync(binaryPath, { force: true })
    },
  }
}
