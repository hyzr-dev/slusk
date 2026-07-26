import { useEffect, useState } from 'react';
import { useFlashMessage } from './FlashContext';
import styles from './StatusBar.module.css';

function useClock(): string {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const id = setInterval(() => setNow(new Date()), 1000);
    return () => clearInterval(id);
  }, []);
  return [now.getHours(), now.getMinutes(), now.getSeconds()]
    .map((n) => String(n).padStart(2, '0'))
    .join(':');
}

/**
 * The bottom rule: transient confirmations and a wall clock.
 *
 * The key-hint section from the design mock is absent on purpose — it would
 * advertise bindings that do not exist yet. It returns with issue #199.
 */
export default function StatusBar() {
  const flash = useFlashMessage();
  const clock = useClock();

  return (
    <div className={styles.bar}>
      <span className={styles.spacer} />
      {flash ? <span className={styles.flash}>{`✓ ${flash}`}</span> : null}
      <span className={styles.clock}>{clock}</span>
    </div>
  );
}
