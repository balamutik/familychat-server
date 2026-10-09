import {useEffect,useRef,useState} from 'react'
import {request,APIError} from './api'

type Invitation={id:string,server_name:string,server_url:string,url:string,expires_at:string,revoked_at:string|null}
const localDate=(date:Date)=>new Date(date.getTime()-date.getTimezoneOffset()*60000).toISOString().slice(0,16)
const failure=(e:unknown)=>e instanceof APIError&&e.code==='invalid_invitation'?'Проверьте адрес, название и срок: от текущего времени до одного года.':'Не удалось выполнить действие. Попробуйте ещё раз.'

export function Invitations({token}:{token:string}) {
 const [items,setItems]=useState<Invitation[]>([]),[name,setName]=useState('Семья'),[address,setAddress]=useState(window.location.origin)
 const [expiry,setExpiry]=useState(()=>localDate(new Date(Date.now()+7*86400000)))
 const [loading,setLoading]=useState(true),[busy,setBusy]=useState(false),[error,setError]=useState(''),[notice,setNotice]=useState('')
 const [now,setNow]=useState(Date.now())
 const generation=useRef(0)
 useEffect(()=>{
  const version=++generation.current,controller=new AbortController()
  request<{invitations:Invitation[]}>('/api/v1/admin/invites','GET',token,undefined,controller.signal)
   .then(result=>{if(version===generation.current)setItems(result.invitations)})
   .catch(e=>{if(!controller.signal.aborted&&version===generation.current)setError(failure(e))})
   .finally(()=>{if(version===generation.current)setLoading(false)})
  const timer=setInterval(()=>setNow(Date.now()),1000)
  return()=>{generation.current++;controller.abort();clearInterval(timer)}
 },[token])
 const create=async(event:React.FormEvent)=>{
  event.preventDefault();const end=new Date(expiry),version=generation.current
  if(!Number.isFinite(end.getTime())||end.getTime()<=Date.now()){setError('Укажите дату и время в будущем.');return}
  setBusy(true);setError('');setNotice('')
  try{
   const result=await request<Invitation>('/api/v1/admin/invites','POST',token,{server_name:name.trim(),server_url:address.trim(),expires_at:end.toISOString()})
   if(version===generation.current){setItems(items=>[result,...items].slice(0,100));setNotice('Приглашение создано. Скопируйте ссылку и отправьте участнику.')}
  }catch(e){if(version===generation.current)setError(failure(e))}finally{if(version===generation.current)setBusy(false)}
 }
 const revoke=async(id:string)=>{
  const version=generation.current;setBusy(true);setError('');setNotice('')
  try{await request(`/api/v1/admin/invites/${id}`,'DELETE',token);if(version===generation.current)setItems(items=>items.map(item=>item.id===id?{...item,revoked_at:new Date().toISOString()}:item))}
  catch(e){if(version===generation.current)setError(failure(e))}finally{if(version===generation.current)setBusy(false)}
 }
 const copy=async(url:string)=>{
  const version=generation.current
  try{await navigator.clipboard.writeText(url);if(version===generation.current)setNotice('Ссылка скопирована.')}
  catch{if(version===generation.current)setNotice('Выделите и скопируйте ссылку из поля вручную.')}
 }
 return <section id="invitations" className="invitations-section">
  <div className="section-head"><div><h2>Приглашения</h2><p>Ссылка добавляет сервер в приложение. Регистрация зависит от настройки выше.</p></div></div>
  <form onSubmit={create} className="invitation-form">
   <label>Название сервера<input value={name} onChange={e=>setName(e.target.value)} maxLength={100} required/></label>
   <label>Адрес сервера<input aria-label="Адрес сервера" type="url" value={address} onChange={e=>setAddress(e.target.value)} placeholder="https://chat.example" required/><small>Адрес, доступный приглашённому пользователю.</small></label>
   <label>Действует до<input aria-label="Действует до" type="datetime-local" value={expiry} onInput={e=>setExpiry(e.currentTarget.value)} onChange={e=>setExpiry(e.target.value)} required/><small>Ваше местное время. Максимальный срок — один год.</small></label>
   <button disabled={busy||loading}>Создать приглашение</button>
  </form>
  {error&&<p role="alert" className="error">{error}</p>}{notice&&<p role="status" className="notice">{notice}</p>}
  {loading?<p>Загрузка приглашений…</p>:items.length===0?<p>Приглашений пока нет.</p>:<ul className="invitation-list">{items.map(item=>{
   const expired=new Date(item.expires_at).getTime()<=now,disabled=!!item.revoked_at||expired
   return <li key={item.id}><div className="invitation-heading"><strong>{item.server_name}</strong><span>{item.revoked_at?'Отозвано':expired?'Срок истёк':`До ${new Date(item.expires_at).toLocaleString()}`}</span></div>
    <input aria-label={`Ссылка приглашения ${item.server_name}`} value={item.url} readOnly onFocus={e=>e.currentTarget.select()}/>
    <div className="invitation-actions"><button type="button" onClick={()=>copy(item.url)} disabled={disabled}>Копировать ссылку</button><button type="button" className="text-button" onClick={()=>revoke(item.id)} disabled={busy||disabled}>Отозвать</button></div>
   </li>
  })}</ul>}
 </section>
}
