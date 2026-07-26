import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';

const SetContext = createContext<(message: string) => void>(() => {});
const ValueContext = createContext<string | null>(null);

const FLASH_MS = 3200;

/**
 * Transient confirmations for actions that have no other visible result —
 * "cancelled #2291" after a mutation the row itself does not immediately
 * reflect, because the next poll is up to 3 s away.
 *
 * Setter and value live in separate contexts so a component that only fires
 * flashes (every mutation button) does not re-render when a message appears.
 */
export function FlashProvider({ children }: { children: ReactNode }) {
  const [message, setMessage] = useState<string | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const flash = useCallback((next: string) => {
    if (timer.current) clearTimeout(timer.current);
    setMessage(next);
    timer.current = setTimeout(() => setMessage(null), FLASH_MS);
  }, []);

  useEffect(() => () => {
    if (timer.current) clearTimeout(timer.current);
  }, []);

  return (
    <SetContext.Provider value={flash}>
      <ValueContext.Provider value={message}>{children}</ValueContext.Provider>
    </SetContext.Provider>
  );
}

export function useFlash() {
  return useContext(SetContext);
}

export function useFlashMessage() {
  return useContext(ValueContext);
}
