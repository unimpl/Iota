const paths = {
  panel: "M9 3v18M4 3h16a1 1 0 0 1 1 1v16a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1Z",
  new: "M12 5v14M5 12h14",
  send: "m6 11 6-6 6 6M12 5v14",
  stop: "M7 7h10v10H7Z",
  close: "m6 6 12 12M18 6 6 18",
  chevron: "m8 10 4 4 4-4",
  check: "m5 12 4 4L19 6",
  copy: "M9 9h11v11H9ZM15 9V4H4v11h5",
  trash: "M3 6h18M9 6V3h6v3M6 6l1 15h10l1-15M10 10v7M14 10v7",
  more: "M5 12h.01M12 12h.01M19 12h.01",
  arrow: "M4 12h16m-6-6 6 6-6 6",
};

export function Icon({ name, size = 20 }: { name: keyof typeof paths; size?: number }) {
  return <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={paths[name]} /></svg>;
}
