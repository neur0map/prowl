// Wrapper for events - provides both named and default exports
import eventsRaw from '../../node_modules/events/events.js';
const events = eventsRaw.default || eventsRaw;

// Re-export as both default and named export for compatibility
export const { EventEmitter } = events;
export default events;
