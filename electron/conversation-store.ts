/**
 * Conversation Store
 *
 * File-based persistence for chat conversations.
 * Stores up to MAX_CONVERSATIONS per project in userData/conversations/.
 */

import { app } from 'electron'
import { join } from 'path'
import { readFile, writeFile, mkdir } from 'fs/promises'
import { createHash } from 'crypto'

export interface StoredMessage {
  id: string
  role: 'user' | 'assistant'
  content: string
  toolSummary?: string
  timestamp: number
}

export interface StoredConversation {
  id: string
  projectPath: string
  title: string
  messages: StoredMessage[]
  compactedSummary?: string
  createdAt: number
  updatedAt: number
}

const MAX_CONVERSATIONS = 5

function getStorePath(): string {
  return join(app.getPath('userData'), 'conversations')
}

function projectKey(projectPath: string): string {
  return createHash('sha256').update(projectPath).digest('hex').slice(0, 16)
}

function storeFile(projectPath: string): string {
  return join(getStorePath(), `${projectKey(projectPath)}.json`)
}

export async function loadConversations(projectPath: string): Promise<StoredConversation[]> {
  try {
    const data = await readFile(storeFile(projectPath), 'utf-8')
    const parsed = JSON.parse(data)
    return Array.isArray(parsed) ? parsed : []
  } catch {
    return []
  }
}

export async function saveConversation(
  projectPath: string,
  conversation: StoredConversation
): Promise<void> {
  const dir = getStorePath()
  await mkdir(dir, { recursive: true })

  let conversations = await loadConversations(projectPath)

  // Update existing or add new
  const idx = conversations.findIndex(c => c.id === conversation.id)
  if (idx >= 0) {
    conversations[idx] = conversation
  } else {
    conversations.push(conversation)
  }

  // Keep only the most recent MAX_CONVERSATIONS
  conversations.sort((a, b) => b.updatedAt - a.updatedAt)
  conversations = conversations.slice(0, MAX_CONVERSATIONS)

  await writeFile(storeFile(projectPath), JSON.stringify(conversations, null, 2), 'utf-8')
}

export async function deleteConversation(
  projectPath: string,
  conversationId: string
): Promise<void> {
  const dir = getStorePath()
  await mkdir(dir, { recursive: true })

  let conversations = await loadConversations(projectPath)
  conversations = conversations.filter(c => c.id !== conversationId)

  await writeFile(storeFile(projectPath), JSON.stringify(conversations, null, 2), 'utf-8')
}
