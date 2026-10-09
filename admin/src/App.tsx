import {Invitations} from './Invitations'
import {useEffect,useRef,useState} from 'react'
import {APIError,request,type Login,type Page,type StorageSettings,type User} from './api'

const message=(e:unknown)=>e instanceof APIError?({invalid_credentials:'Неверный логин или пароль',forbidden:'Недостаточно прав',last_admin:'Нельзя заблокировать последнего администратора',login_exists:'Логин уже занят',invalid_credentials_input:'Проверьте логин и пароль',invalid_setting:'Проверьте параметры хранения'}[e.code]||`Ошибка сервера: ${e.code}`):'Не удалось связаться с сервером'

export default function App() {
  const [token,setToken]=useState(''),[self,setSelf]=useState<User|null>(null),[login,setLogin]=useState(''),[password,setPassword]=useState('')
  const [users,setUsers]=useState<User[]>([]),[cursor,setCursor]=useState(''),[search,setSearch]=useState('')
  const [registration,setRegistration]=useState(false),[loading,setLoading]=useState(false),[saving,setSaving]=useState(false),[error,setError]=useState(''),[notice,setNotice]=useState('')
  const [storage,setStorage]=useState<StorageSettings|null>(null),[storageDays,setStorageDays]=useState('0'),[storageMiB,setStorageMiB]=useState('250'),[storageSaving,setStorageSaving]=useState(false)
  const [newLogin,setNewLogin]=useState(''),[newPassword,setNewPassword]=useState('')
  const abort=useRef<AbortController|null>(null),generation=useRef(0)
  const reset=()=>{generation.current++;abort.current?.abort();abort.current=null;setToken('');setSelf(null);setUsers([]);setCursor('');setStorage(null);setStorageDays('0');setStorageMiB('250');setError('');setNotice('');setLoading(false);setSaving(false);setStorageSaving(false)}
  const logout=async()=>{const old=token;reset();request('/api/v1/auth/logout','POST',old).catch(()=>{})}

  useEffect(()=>{
    if (!token || self?.role!=='admin') return
    const current=++generation.current;abort.current?.abort();const controller=new AbortController();abort.current=controller
    setLoading(true);setError('')
    Promise.all([request<Page>(`/api/v1/admin/users?limit=30&q=${encodeURIComponent(search.trim())}`,'GET',token,undefined,controller.signal),request<{enabled:boolean}>('/api/v1/admin/settings/registration','GET',token,undefined,controller.signal),request<StorageSettings>('/api/v1/admin/settings/storage','GET',token,undefined,controller.signal)]).then(([page,setting,storageSetting])=>{
      if (generation.current!==current) return
      setUsers(page.users);setCursor(page.next_cursor);setRegistration(setting.enabled)
      if(!storage){setStorage(storageSetting);setStorageDays(String(storageSetting.retention_days));setStorageMiB(String(Math.floor(storageSetting.max_file_bytes/(1024*1024))))}
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
  const saveStorage=async(e:React.FormEvent)=>{
    e.preventDefault();if(!storage)return
    const current=generation.current,days=Number(storageDays),mib=Number(storageMiB),bytes=mib*1024*1024
    if(!Number.isInteger(days)||days<0||days>3650||!Number.isInteger(mib)||mib<1||!Number.isSafeInteger(bytes)||bytes>storage.max_allowed_bytes){setError('Укажите срок от 0 до 3650 дней и размер в пределах серверного лимита');return}
    setStorageSaving(true);setError('');setNotice('')
    try{const saved=await request<StorageSettings>('/api/v1/admin/settings/storage','PATCH',token,{retention_days:days,max_file_bytes:bytes});if(current!==generation.current)return;setStorage(saved);setStorageDays(String(saved.retention_days));setStorageMiB(String(Math.floor(saved.max_file_bytes/(1024*1024))));setNotice('Параметры хранения сохранены')}
    catch(e){if(current===generation.current)setError(message(e))}
    finally{if(current===generation.current)setStorageSaving(false)}
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

  return <div className="app-shell"><aside className="sidebar"><div className="sidebar-head"><div className="small-mark" aria-hidden="true"><span/><span/><span/></div><strong>FamilyChat</strong></div><nav aria-label="Разделы"><a href="#access">Доступ</a><a href="#invitations">Приглашения</a><a href="#people">Участники</a><a href="#storage">Хранение</a></nav><div className="sidebar-bottom"><span>{self.login}</span><button className="text-button" onClick={logout}>Выйти</button></div></aside><main className="content"><header className="page-head"><p>Управление семьёй</p><h1>Доступ к чату</h1><span>Аккаунты, регистрация и хранение файлов</span></header>{error&&<p className="error" role="alert">{error}</p>}{notice&&<p className="notice" role="status">{notice}</p>}<section id="access" className="access-section"><div><h2>Самостоятельная регистрация</h2><p>Новые участники смогут создать аккаунт с логином и паролем. Доступ к чатам появится только после входа и добавления в чат.</p></div><button className={'switch '+(registration?'on':'')} role="switch" aria-checked={registration} aria-label="Самостоятельная регистрация" onClick={toggle} disabled={saving}><span/></button><strong>{registration?'Включена':'Выключена'}</strong></section><Invitations token={token}/><section id="people" className="people-section"><div className="section-head"><div><h2>Участники</h2><p>Аккаунты, которым разрешён вход на сервер</p></div><input className="search" aria-label="Поиск участника" placeholder="Найти по логину" value={search} onChange={e=>setSearch(e.target.value)}/></div><div className="table-wrap"><table><thead><tr><th>Логин</th><th>Роль</th><th>Состояние</th><th>Действие</th></tr></thead><tbody>{users.map(user=><tr key={user.id}><td><strong>{user.login}</strong></td><td>{user.role==='admin'?'Администратор':'Участник'}</td><td><span className={'status '+(user.disabled?'blocked':'active')}>{user.disabled?'Доступ закрыт':'Активен'}</span></td><td><button className="row-action" onClick={()=>changeUser(user)} disabled={saving||user.id===self.id}>{user.disabled?'Восстановить':'Заблокировать'}</button></td></tr>)}</tbody></table>{loading&&<p className="table-message">Загрузка участников…</p>}{!loading&&users.length===0&&<p className="table-message">Участники не найдены. Проверьте запрос или создайте аккаунт ниже.</p>}</div>{cursor&&<button className="more" onClick={more} disabled={loading}>Показать ещё</button>}</section><section className="create-section"><div><h2>Добавить участника</h2><p>Создайте аккаунт, если самостоятельная регистрация выключена.</p></div><form onSubmit={create}><label>Логин<input value={newLogin} onChange={e=>setNewLogin(e.target.value)} minLength={3} maxLength={32} pattern="[A-Za-z0-9_]+" required/></label><label>Пароль<input type="password" value={newPassword} onChange={e=>setNewPassword(e.target.value)} minLength={12} required/></label><button disabled={saving}>Создать аккаунт</button></form></section><section id="storage" className="storage-section"><div className="section-head"><div><h2>Хранение файлов</h2><p>Срок хранения и предел размера новой загрузки</p></div></div>{storage?<form onSubmit={saveStorage}><div className="storage-fields"><label>Хранить файл, дней<input type="number" min="0" max="3650" step="1" value={storageDays} onChange={e=>setStorageDays(e.target.value)} required/><small>0 — бессрочно. Срок применяется и к уже загруженным файлам.</small></label><label>Максимальный размер файла, МиБ<input type="number" min="1" max={Math.floor(storage.max_allowed_bytes/(1024*1024))} step="1" value={storageMiB} onChange={e=>setStorageMiB(e.target.value)} required/><small>Серверный предел: {Math.floor(storage.max_allowed_bytes/(1024*1024))} МиБ. Изменение влияет на новые загрузки.</small></label></div><p className="storage-warning">Если сократить срок, старые файлы сразу станут недоступны. Worker удалит их из хранилища; сообщения останутся в истории.</p><button className="storage-save" disabled={storageSaving}>{storageSaving?'Сохранение…':'Сохранить настройки'}</button></form>:<p>Загрузка настроек хранения…</p>}</section></main></div>
}
