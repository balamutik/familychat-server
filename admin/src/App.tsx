import {useEffect,useRef,useState} from 'react'
import {APIError,request,type Login,type Page,type User} from './api'

const message=(e:unknown)=>e instanceof APIError?({invalid_credentials:'Неверный логин или пароль',forbidden:'Недостаточно прав',last_admin:'Нельзя заблокировать последнего администратора',login_exists:'Логин уже занят',invalid_credentials_input:'Проверьте логин и пароль'}[e.code]||`Ошибка сервера: ${e.code}`):'Не удалось связаться с сервером'

export default function App() {
  const [token,setToken]=useState(''),[self,setSelf]=useState<User|null>(null),[login,setLogin]=useState(''),[password,setPassword]=useState('')
  const [users,setUsers]=useState<User[]>([]),[cursor,setCursor]=useState(''),[search,setSearch]=useState('')
  const [registration,setRegistration]=useState(false),[loading,setLoading]=useState(false),[saving,setSaving]=useState(false),[error,setError]=useState(''),[notice,setNotice]=useState('')
  const [newLogin,setNewLogin]=useState(''),[newPassword,setNewPassword]=useState('')
  const abort=useRef<AbortController|null>(null),generation=useRef(0)
  const reset=()=>{generation.current++;abort.current?.abort();abort.current=null;setToken('');setSelf(null);setUsers([]);setCursor('');setError('');setNotice('');setLoading(false);setSaving(false)}
  const logout=async()=>{const old=token;reset();request('/api/v1/auth/logout','POST',old).catch(()=>{})}

  useEffect(()=>{
    if (!token || self?.role!=='admin') return
    const current=++generation.current;abort.current?.abort();const controller=new AbortController();abort.current=controller
    setLoading(true);setError('')
    Promise.all([request<Page>(`/api/v1/admin/users?limit=30&q=${encodeURIComponent(search.trim())}`,'GET',token,undefined,controller.signal),request<{enabled:boolean}>('/api/v1/admin/settings/registration','GET',token,undefined,controller.signal)]).then(([page,setting])=>{
      if (generation.current!==current) return
      setUsers(page.users);setCursor(page.next_cursor);setRegistration(setting.enabled)
    }).catch(e=>{if (generation.current===current && !controller.signal.aborted) setError(message(e))}).finally(()=>{if(generation.current===current)setLoading(false)})
    return()=>controller.abort()
  },[token,self?.role,search])

  const signIn=async(e:React.FormEvent)=>{
    e.preventDefault();setError('');setLoading(true)
    try { const result=await request<Login>('/api/v1/auth/login','POST','',{login,password});if(result.user.role!=='admin'){await request('/api/v1/auth/logout','POST',result.token);setError('Этот аккаунт не является администратором');return} setToken(result.token);setSelf(result.user);setPassword('') }
    catch(e){setError(message(e))} finally{setLoading(false)}
  }
  const toggle=async()=>{
    const current=generation.current;setSaving(true);setError('')
    try{const next=await request<{enabled:boolean}>('/api/v1/admin/settings/registration','PATCH',token,{enabled:!registration});if(current!==generation.current)return;setRegistration(next.enabled);setNotice(next.enabled?'Самостоятельная регистрация включена':'Самостоятельная регистрация выключена')}
    catch(e){if(current===generation.current)setError(message(e))}
    finally{if(current===generation.current)setSaving(false)}
  }
  const create=async(e:React.FormEvent)=>{
    e.preventDefault();const current=generation.current;setSaving(true);setError('')
    try{await request('/api/v1/admin/users','POST',token,{login:newLogin,password:newPassword});if(current!==generation.current)return;setNewLogin('');setNewPassword('');setNotice('Аккаунт создан');setSearch('');const page=await request<Page>('/api/v1/admin/users?limit=30','GET',token);if(current!==generation.current)return;setUsers(page.users);setCursor(page.next_cursor)}
    catch(e){if(current===generation.current)setError(message(e))}
    finally{if(current===generation.current)setSaving(false)}
  }
  const changeUser=async(user:User)=>{
    const current=generation.current;setSaving(true);setError('')
    try{await request(`/api/v1/admin/users/${user.id}`,'PATCH',token,{disabled:!user.disabled});if(current!==generation.current)return;setUsers(list=>list.map(item=>item.id===user.id?{...item,disabled:!item.disabled}:item));setNotice(user.disabled?'Доступ восстановлен':'Доступ закрыт')}
    catch(e){if(current===generation.current)setError(message(e))}
    finally{if(current===generation.current)setSaving(false)}
  }
  const more=async()=>{
    if(!cursor)return;const current=generation.current;setLoading(true)
    try{const page=await request<Page>(`/api/v1/admin/users?limit=30&q=${encodeURIComponent(search.trim())}&cursor=${encodeURIComponent(cursor)}`,'GET',token);if(current!==generation.current)return;setUsers(list=>[...list,...page.users]);setCursor(page.next_cursor)}
    catch(e){if(current===generation.current)setError(message(e))}
    finally{if(current===generation.current)setLoading(false)}
  }

  if(!token || !self) return <main className="login-shell"><div className="brand-mark" aria-hidden="true"><span/><span/><span/></div><section className="login-panel"><p className="intro">Домашнее пространство</p><h1>FamilyChat</h1><p className="subtitle">Управление доступом</p><form onSubmit={signIn}><label>Логин<input autoComplete="username" value={login} onChange={e=>setLogin(e.target.value)} required/></label><label>Пароль<input type="password" autoComplete="current-password" value={password} onChange={e=>setPassword(e.target.value)} required/></label><button disabled={loading}>{loading?'Вход…':'Войти'}</button></form>{error&&<p className="error" role="alert">{error}</p>}<p className="login-note">Доступно только администратору семьи.</p></section></main>

  return <div className="app-shell"><aside className="sidebar"><div className="sidebar-head"><div className="small-mark" aria-hidden="true"><span/><span/><span/></div><strong>FamilyChat</strong></div><nav aria-label="Разделы"><a href="#access">Доступ</a><a href="#people">Участники</a></nav><div className="sidebar-bottom"><span>{self.login}</span><button className="text-button" onClick={logout}>Выйти</button></div></aside><main className="content"><header className="page-head"><p>Управление семьёй</p><h1>Доступ к чату</h1><span>Настройки аккаунтов и самостоятельной регистрации</span></header>{error&&<p className="error" role="alert">{error}</p>}{notice&&<p className="notice" role="status">{notice}</p>}<section id="access" className="access-section"><div><h2>Самостоятельная регистрация</h2><p>Новые участники смогут создать аккаунт с логином и паролем. Доступ к чатам появится только после входа и добавления в чат.</p></div><button className={'switch '+(registration?'on':'')} role="switch" aria-checked={registration} aria-label="Самостоятельная регистрация" onClick={toggle} disabled={saving}><span/></button><strong>{registration?'Включена':'Выключена'}</strong></section><section id="people" className="people-section"><div className="section-head"><div><h2>Участники</h2><p>Аккаунты, которым разрешён вход на сервер</p></div><input className="search" aria-label="Поиск участника" placeholder="Найти по логину" value={search} onChange={e=>setSearch(e.target.value)}/></div><div className="table-wrap"><table><thead><tr><th>Логин</th><th>Роль</th><th>Состояние</th><th>Действие</th></tr></thead><tbody>{users.map(user=><tr key={user.id}><td><strong>{user.login}</strong></td><td>{user.role==='admin'?'Администратор':'Участник'}</td><td><span className={'status '+(user.disabled?'blocked':'active')}>{user.disabled?'Доступ закрыт':'Активен'}</span></td><td><button className="row-action" onClick={()=>changeUser(user)} disabled={saving||user.id===self.id}>{user.disabled?'Восстановить':'Заблокировать'}</button></td></tr>)}</tbody></table>{loading&&<p className="table-message">Загрузка участников…</p>}{!loading&&users.length===0&&<p className="table-message">Участники не найдены. Проверьте запрос или создайте аккаунт ниже.</p>}</div>{cursor&&<button className="more" onClick={more} disabled={loading}>Показать ещё</button>}</section><section className="create-section"><div><h2>Добавить участника</h2><p>Создайте аккаунт, если самостоятельная регистрация выключена.</p></div><form onSubmit={create}><label>Логин<input value={newLogin} onChange={e=>setNewLogin(e.target.value)} minLength={3} maxLength={32} pattern="[A-Za-z0-9_]+" required/></label><label>Пароль<input type="password" value={newPassword} onChange={e=>setNewPassword(e.target.value)} minLength={12} required/></label><button disabled={saving}>Создать аккаунт</button></form></section></main></div>
}
