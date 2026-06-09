import { EventEmitter } from 'events';

export class TextProcessor extends EventEmitter {
  constructor(options = {}) {
    super();
    this.minChunkLength = options.minChunkLength || 10;
    this.maxChunkLength = options.maxChunkLength || 500;
    this.flushDelay = options.flushDelay || 1500; // ms to wait before flushing incomplete buffer
    this.buffer = '';
    this.inCodeBlock = false;
    this.seq = 0;
    this._flushTimer = null;
  }

  feed(text) {
    if (!text || typeof text !== 'string') return;

    // Strip fenced code blocks (``` ... ```), keeping only the prose OUTSIDE
    // them. We split on ``` and walk the segments, flipping in/out of a code
    // block at each fence. This is correct whether a block is split across
    // feeds (the inCodeBlock flag carries state between calls) or arrives whole
    // in a single feed — a complete block within one feed has an even number of
    // fences, so the old "toggle then drop the whole chunk" logic spoke its body
    // aloud. Here the in-block segments (including the opening ```js language
    // tag) are simply omitted.
    const segments = text.split('```');
    let visible = '';
    for (let i = 0; i < segments.length; i++) {
      if (!this.inCodeBlock) visible += segments[i];
      if (i < segments.length - 1) this.inCodeBlock = !this.inCodeBlock;
    }

    // Nothing speakable in this chunk (all code/empty) — nothing to buffer.
    if (!visible.trim()) return;

    // Filter out tool/noise content
    if (this._isNoise(visible)) return;

    this.buffer += visible;
    this._tryFlush();
    this._scheduleFlush();
  }

  // Force flush remaining buffer (e.g., on session end or pause)
  flush() {
    this._clearFlushTimer();
    if (this.buffer.trim().length >= this.minChunkLength) {
      this._emitSentence(this.buffer.trim());
    }
    this.buffer = '';
  }

  reset() {
    this._clearFlushTimer();
    this.buffer = '';
    this.inCodeBlock = false;
    this.seq = 0;
  }

  _scheduleFlush() {
    this._clearFlushTimer();
    this._flushTimer = setTimeout(() => {
      this.flush();
    }, this.flushDelay);
  }

  _clearFlushTimer() {
    if (this._flushTimer) {
      clearTimeout(this._flushTimer);
      this._flushTimer = null;
    }
  }

  _tryFlush() {
    // Split at sentence boundaries
    const sentenceEnd = /([.!?])\s+/g;
    let match;
    let lastIndex = 0;

    while ((match = sentenceEnd.exec(this.buffer)) !== null) {
      const sentence = this.buffer.slice(lastIndex, match.index + 1).trim();
      if (sentence.length >= this.minChunkLength) {
        this._emitSentence(sentence);
      }
      lastIndex = match.index + match[0].length;
    }

    if (lastIndex > 0) {
      this.buffer = this.buffer.slice(lastIndex);
    }

    // Force flush if buffer exceeds max
    if (this.buffer.length >= this.maxChunkLength) {
      // Try to break at the last space
      const lastSpace = this.buffer.lastIndexOf(' ', this.maxChunkLength);
      const breakAt = lastSpace > this.minChunkLength ? lastSpace : this.maxChunkLength;
      const chunk = this.buffer.slice(0, breakAt).trim();
      this.buffer = this.buffer.slice(breakAt);
      if (chunk.length >= this.minChunkLength) {
        this._emitSentence(chunk);
      }
    }
  }

  _emitSentence(text) {
    // Clean the text for speech
    const cleaned = this._cleanForSpeech(text);
    if (cleaned.length < this.minChunkLength) return;

    this.seq++;
    this.emit('sentence', { seq: this.seq, text: cleaned });
  }

  _cleanForSpeech(text) {
    return text
      // Remove markdown formatting
      .replace(/\*\*(.+?)\*\*/g, '$1')
      .replace(/\*(.+?)\*/g, '$1')
      .replace(/_(.+?)_/g, '$1')
      .replace(/`([^`]+)`/g, '$1')
      // Remove URLs
      .replace(/https?:\/\/\S+/g, '')
      // Remove file paths like /src/foo/bar.js
      .replace(/(?:^|\s)[\/~][\w.\-\/]+/g, ' ')
      // Collapse whitespace
      .replace(/\s+/g, ' ')
      .trim();
  }

  _isNoise(text) {
    const trimmed = text.trim();
    // Skip empty or whitespace-only
    if (!trimmed) return true;
    // Skip JSON-like content
    if (trimmed.startsWith('{') && trimmed.endsWith('}')) return true;
    // Skip tool call markers
    if (trimmed.startsWith('Tool:') || trimmed.startsWith('tool_use')) return true;
    // Skip file path only lines
    if (/^[\/~][\w.\-\/]+:\d+$/.test(trimmed)) return true;
    return false;
  }
}
