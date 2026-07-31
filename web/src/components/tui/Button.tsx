import type { ReactNode } from 'react';
import styles from './Button.module.css';

interface Props {
  variant?: 'primary' | 'ghost' | 'danger' | 'identified';
  onClick?: () => void;
  // Passed straight through to the <button>; used by the jobs list's
  // two-click delete confirm to disarm itself when focus leaves the button,
  // rather than every caller reaching past this component for that.
  onBlur?: () => void;
  disabled?: boolean;
  type?: 'button' | 'submit';
  children: ReactNode;
}

export default function Button({
  variant = 'ghost',
  onClick,
  onBlur,
  disabled = false,
  type = 'button',
  children,
}: Props) {
  return (
    <button
      type={type}
      className={`${styles.btn} ${styles[variant]}`}
      onClick={onClick}
      onBlur={onBlur}
      disabled={disabled}
    >
      {children}
    </button>
  );
}
