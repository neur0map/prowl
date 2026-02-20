import * as pty from 'node-pty'
import { BrowserWindow } from 'electron'
import { homedir } from 'os'

export interface TerminalInstance {
  id: string
  pty: pty.IPty
  name: string
}

export class TerminalManager {
  private terminals = new Map<string, TerminalInstance>()
  private window: BrowserWindow | null = null
  private counter = 0

  setWindow(win: BrowserWindow): void {
    this.window = win
  }

  create(cwd?: string): string {
    const id = `term-${++this.counter}`
    const shell = process.env.SHELL || (process.platform === 'win32' ? 'powershell.exe' : '/bin/bash')
    const resolvedCwd = cwd || homedir()

    // Clean env: remove vars that conflict with nvm/shell init
    const cleanEnv = { ...process.env }
    delete cleanEnv.npm_config_prefix
    delete cleanEnv.npm_config_loglevel
    delete cleanEnv.NODE_ENV

    const ptyProcess = pty.spawn(shell, [], {
      name: 'xterm-256color',
      cols: 80,
      rows: 24,
      cwd: resolvedCwd,
      env: {
        ...cleanEnv,
        TERM: 'xterm-256color',
        COLORTERM: 'truecolor',
      } as Record<string, string>,
    })

    // Detect process name from the first output or the shell name
    const name = shell.split('/').pop() || 'shell'

    const instance: TerminalInstance = { id, pty: ptyProcess, name }
    this.terminals.set(id, instance)

    ptyProcess.onData((data) => {
      this.window?.webContents.send('terminal:data', { id, data })
    })

    ptyProcess.onExit(({ exitCode }) => {
      this.window?.webContents.send('terminal:exit', { id, exitCode })
      this.terminals.delete(id)
    })

    return id
  }

  write(id: string, data: string): void {
    this.terminals.get(id)?.pty.write(data)
  }

  resize(id: string, cols: number, rows: number): void {
    const term = this.terminals.get(id)
    if (term) {
      try {
        term.pty.resize(cols, rows)
      } catch {
        // Ignore resize errors on dead PTYs
      }
    }
  }

  kill(id: string): void {
    const term = this.terminals.get(id)
    if (term) {
      term.pty.kill()
      this.terminals.delete(id)
    }
  }

  getTitle(id: string): string {
    const term = this.terminals.get(id)
    const raw = term?.pty.process || term?.name || 'shell'
    return raw.split('/').pop() || 'shell'
  }

  killAll(): void {
    for (const [id, term] of this.terminals) {
      term.pty.kill()
      this.terminals.delete(id)
    }
  }
}
