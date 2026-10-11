import { useEffect, useState } from 'react'
import { navigate, usePath } from './router.js'
import paw from './paw.svg'

function Link({ to, children }) {
  const onClick = (e) => {
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return
    e.preventDefault()
    navigate(to)
  }
  return <a href={to} onClick={onClick} aria-current={location.pathname === to ? 'page' : undefined}>{children}</a>
}

function useJSON(url) {
  const [state, setState] = useState({ loading: true })
  useEffect(() => {
    let live = true
    setState({ loading: true })
    fetch(url)
      .then(async (r) => (r.ok ? r.json() : Promise.reject(new Error((await r.json()).error))))
      .then((data) => live && setState({ data }), (error) => live && setState({ error }))
    return () => { live = false }
  }, [url])
  return state
}

function Pets() {
  const { data, error, loading } = useJSON('/api/pets')
  if (loading) return <p>Loading…</p>
  if (error) return <p className="error">{error.message}</p>
  return (
    <ul className="pets">
      {data.map((p) => <li key={p.id}><Link to={`/pets/${p.id}`}>{p.name}</Link> <span>{p.kind}</span></li>)}
    </ul>
  )
}

function Pet({ id }) {
  const { data, error, loading } = useJSON(`/api/pets/${id}`)
  if (loading) return <p>Loading…</p>
  if (error) return <p className="error">{error.message}</p>
  return <p className="pet">{data.name} is a {data.kind}, pet #{data.id}.</p>
}

export default function App() {
  const path = usePath()
  const pet = path.match(/^\/pets\/(\d+)$/)
  let page
  if (path === '/') page = <><h1>Pets</h1><Pets /></>
  else if (pet) page = <><h1>Pet {pet[1]}</h1><Pet id={pet[1]} /><Link to="/">All pets</Link></>
  else if (path === '/about') page = <><h1>About</h1><p>A React app served from a Vuka binary.</p></>
  else page = <><h1>Not found</h1><p>No page at {path}.</p></>
  return (
    <div className="shell">
      <header>
        <img src={paw} alt="" width="28" height="28" />
        <strong>{import.meta.env.APP_NAME ?? 'Pet shop'}</strong>
        <nav><Link to="/">Pets</Link><Link to="/about">About</Link></nav>
      </header>
      <main>{page}</main>
    </div>
  )
}
