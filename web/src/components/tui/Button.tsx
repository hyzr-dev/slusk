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
  // Appended after the variant class, for the rare caller that needs layout
  // (not colour/border — that stays variant-driven) beyond what this
  // component itself provides, e.g. IdentifyModal's full-width search button.
  className?: string;
  // Passed straight through to the <button>. Used to expose WHY a button is
  // disabled (e.g. "Enter an album to search.") as a native tooltip, since a
  // disabled control is not reachable by a screen reader's virtual cursor
  // and `disabled` alone carries no reason.
  title?: string;
  children: ReactNode;
}

export default function Button({
  variant = 'ghost',
  onClick,
  onBlur,
  disabled = false,
  type = 'button',
  className = '',
  title,
  children,
}: Props) {
  return (
    <button
      type={type}
      className={`${styles.btn} ${styles[variant]}${className ? ` ${className}` : ''}`}
      onClick={onClick}
      onBlur={onBlur}
      disabled={disabled}
      title={title}
    >
      {children}
    </button>
  );
}
