import { useEffect, useRef, type ReactNode } from 'react'

/**
 * A dialog built on `<dialog>`.
 *
 * The native element rather than a portal and a keydown listener because it
 * brings the things a hand-rolled modal gets wrong: focus is trapped, the rest
 * of the page is inert, Escape closes it, and the backdrop is a real
 * pseudo-element instead of a div with a z-index.
 */
export function Modal({
  open,
  onClose,
  title,
  children,
  footer,
}: {
  open: boolean
  onClose: () => void
  title: string
  children: ReactNode
  footer?: ReactNode
}) {
  const ref = useRef<HTMLDialogElement>(null)

  useEffect(() => {
    const dialog = ref.current
    if (!dialog) return
    if (open && !dialog.open) dialog.showModal()
    if (!open && dialog.open) dialog.close()
  }, [open])

  return (
    <dialog
      ref={ref}
      // Escape fires `cancel`, and the close has to be routed back through the
      // caller's state or the next `open` would find the dialog already closed
      // and never reopen it.
      onCancel={(event) => {
        event.preventDefault()
        onClose()
      }}
      onClose={onClose}
      className="w-[min(32rem,calc(100vw-2rem))] rounded-xl p-0 backdrop:bg-slate-900/40"
      aria-label={title}
    >
      <div className="border-b border-slate-200 px-5 py-4">
        <h2 className="text-base font-semibold text-slate-900">{title}</h2>
      </div>
      <div className="px-5 py-4">{children}</div>
      {footer && (
        <div className="flex justify-end gap-2 border-t border-slate-200 bg-slate-50 px-5 py-3">
          {footer}
        </div>
      )}
    </dialog>
  )
}
