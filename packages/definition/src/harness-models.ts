// Harness model catalog. Port of internal/harness/models.go. A copy lives
// here until the catalog moves to packages/core.

import { goQuote } from './gofmt.ts';
import type { HarnessType } from './types.ts';

export interface Model {
  id: string;
  label: string;
  /** anthropic | openai */
  provider: string;
  default?: boolean;
  reasoning_levels?: string[];
}

const claudeReasoningLevels = ['low', 'medium', 'high', 'xhigh', 'max'];
const codexReasoningLevels = ['minimal', 'low', 'medium', 'high', 'xhigh'];

function claudeCatalog(): Model[] {
  const m = (id: string, label: string, def: boolean): Model => {
    const out: Model = { id, label, provider: 'anthropic', reasoning_levels: [...claudeReasoningLevels] };
    if (def) out.default = true;
    return out;
  };
  return [
    m('claude-opus-5-5', 'Claude Opus 5.5', true),
    m('claude-sonnet-5-5', 'Claude Sonnet 5.5', false),
    m('claude-haiku-5-5', 'Claude Haiku 5.5', false),
    m('claude-fable-5-1', 'Claude Fable 5.1', false),
    m('claude-opus-5', 'Claude Opus 5', false),
    m('claude-sonnet-5', 'Claude Sonnet 5', false),
    m('claude-opus-4-8', 'Claude Opus 4.8', false),
    m('claude-opus-4-7', 'Claude Opus 4.7', false),
    m('claude-opus-4-6', 'Claude Opus 4.6', false),
    m('claude-sonnet-4-6', 'Claude Sonnet 4.6', false),
    m('claude-haiku-4-5', 'Claude Haiku 4.5', false),
    m('opus', 'Opus (latest alias)', false),
    m('sonnet', 'Sonnet (latest alias)', false),
    m('haiku', 'Haiku (latest alias)', false),
  ];
}

function codexCatalog(): Model[] {
  const m = (id: string, label: string, def: boolean): Model => {
    const out: Model = { id, label, provider: 'openai', reasoning_levels: [...codexReasoningLevels] };
    if (def) out.default = true;
    return out;
  };
  return [
    m('gpt-5.3-codex', 'GPT-5.3 Codex', true),
    m('gpt-5.2-codex', 'GPT-5.2 Codex', false),
    m('gpt-5.1-codex-max', 'GPT-5.1 Codex Max', false),
    m('gpt-5.1-codex-mini', 'GPT-5.1 Codex Mini', false),
    m('gpt-5.2', 'GPT-5.2', false),
    m('gpt-5.1', 'GPT-5.1', false),
    m('gpt-5-codex', 'GPT-5 Codex', false),
    m('gpt-5', 'GPT-5', false),
  ];
}

/** normalizeType maps harness aliases to the canonical type ("claude-code" → "claude", "" → "claude"). */
export function normalizeType(t: string): HarnessType | string {
  const s = t.trim().toLowerCase();
  switch (s) {
    case '':
    case 'claude':
    case 'claude-code':
    case 'claude_code':
    case 'claudecode':
      return 'claude';
    case 'codex':
    case 'codex-cli':
      return 'codex';
  }
  return s;
}

/** models returns the model catalog of a harness type (aliases accepted). */
export function models(t: string): Model[] {
  switch (normalizeType(t)) {
    case 'claude':
      return claudeCatalog();
    case 'codex':
      return codexCatalog();
  }
  return [];
}

/** defaultModel returns the catalog default of t, or "". */
export function defaultModel(t: string): string {
  return models(t).find((m) => m.default)?.id ?? '';
}

/** reasoningLevels returns the accepted reasoningLevel values of t (null for an unknown harness). */
export function reasoningLevels(t: string): string[] | null {
  switch (normalizeType(t)) {
    case 'claude':
      return [...claudeReasoningLevels];
    case 'codex':
      return [...codexReasoningLevels];
  }
  return null;
}

/** validateReasoningLevel returns an error message, or null when level is valid for t. */
export function validateReasoningLevel(t: string, level: string): string | null {
  if (level === '') return null;
  const levels = reasoningLevels(t);
  if (levels === null) return `harness ${goQuote(normalizeType(t))} does not accept reasoningLevel`;
  if (levels.some((l) => l.toLowerCase() === level.toLowerCase())) return null;
  return `invalid reasoningLevel ${goQuote(level)} for harness ${goQuote(normalizeType(t))} (allowed: ${levels.join(', ')})`;
}

/**
 * harnessForModel selects the harness of a model name for the shorthand
 * `model: X`: a Claude model selects claude, an OpenAI model selects codex,
 * any other name gives "".
 */
export function harnessForModel(model: string): HarnessType {
  let m = model.trim().toLowerCase();
  if (m.startsWith('anthropic.')) m = m.slice('anthropic.'.length);
  if (m.startsWith('claude-') || m === 'opus' || m === 'sonnet' || m === 'haiku') return 'claude';
  if (m.startsWith('gpt-') || m.startsWith('codex-') || m.startsWith('chatgpt-')) return 'codex';
  if (m.length >= 2 && m[0] === 'o' && m[1]! >= '0' && m[1]! <= '9') return 'codex';
  return '';
}
