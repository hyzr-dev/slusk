import type { ReactNode } from 'react';
import styles from './Button.module.css';

interface Props {
  variant?: 'primary' | 'ghost' | 'danger';
  onClick?: () => void;
  disabled?: boolean;
  type?: 'button' | 'submit';
  children: ReactNode;
}

export default function Button({
  variant = 'ghost',
  onClick,
  disabled = false,
  type = 'button',
  children,
}: Props) {
  return (
    <button
      type={type}
      className={`${styles.btn} ${styles[variant]}`}
      onClick={onClick}
      disabled={disabled}
    >
      {children}
    </button>
  );
}
