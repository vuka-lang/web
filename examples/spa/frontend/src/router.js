import { useEffect, useState } from 'react'

// A small client-side router: links push history, Back and Forward work, and
// the app serves index.html for every route, so a reload lands here too.
export function usePath() {
  const [path, setPath] = useState(location.pathname)
  useEffect(() => {
    const update = () => setPath(location.pathname)
    addEventListener('popstate', update)
    return () => removeEventListener('popstate', update)
  }, [])
  return path
}

export function navigate(to) {
  history.pushState(null, '', to)
  dispatchEvent(new PopStateEvent('popstate'))
}
