// @vitest-environment jsdom
import {afterEach,expect,it,vi} from 'vitest'
import {cleanup,fireEvent,render,screen,waitFor} from '@testing-library/react'
import App from './App'

afterEach(()=>{cleanup();vi.unstubAllGlobals()})

function response(body:unknown,status=200){return {ok:status>=200&&status<300,status,json:async()=>body}}

it('logs in as administrator, manages registration and blocks a user',async()=>{
  const user={id:'00000000-0000-4000-8000-000000000001',login:'mother',role:'admin',disabled:false,created_at:''}
  const member={id:'00000000-0000-4000-8000-000000000002',login:'member',role:'user',disabled:false,created_at:''}
  const calls:{path:string,method:string,authorization:string}[]=[]
  let savedStorage={retention_days:0,max_file_bytes:250*1024*1024,max_allowed_bytes:250*1024*1024}
  vi.stubGlobal('fetch',vi.fn(async(path:string,init:RequestInit)=>{
    const method=init.method||'GET',authorization=new Headers(init.headers).get('Authorization')||''
    calls.push({path,method,authorization})
    if(path==='/api/v1/auth/login')return response({token:'test-token',user})
    if(path.startsWith('/api/v1/admin/users?'))return response({users:[user,member],next_cursor:''})
    if(path==='/api/v1/admin/settings/registration'&&method==='GET')return response({enabled:false})
    if(path==='/api/v1/admin/settings/registration'&&method==='PATCH')return response({enabled:true})
    if(path==='/api/v1/admin/settings/storage'&&method==='GET')return response(savedStorage)
    if(path==='/api/v1/admin/settings/storage'&&method==='PATCH'){savedStorage={...savedStorage,...JSON.parse(String(init.body))};return response(savedStorage)}
    if(path.endsWith('/admin/users/'+member.id)&&method==='PATCH')return response({id:member.id,disabled:true})
    return response({error:'unexpected'},500)
  }))
  render(<App/>)
  fireEvent.change(screen.getByRole('textbox',{name:'Логин'}),{target:{value:'mother'}})
  fireEvent.change(screen.getByLabelText('Пароль'),{target:{value:'password'}})
  fireEvent.click(screen.getByRole('button',{name:'Войти'}))
  await screen.findByRole('heading',{name:'Доступ к чату'})
  await screen.findByText('member')
  expect(calls.find(c=>c.path.includes('/admin/users?'))?.authorization).toBe('Bearer test-token')
  fireEvent.change(screen.getByRole('spinbutton',{name:/Хранить файл, дней/}),{target:{value:'30'}})
  fireEvent.change(screen.getByRole('spinbutton',{name:/Максимальный размер файла/}),{target:{value:'50'}})
  fireEvent.click(screen.getByRole('button',{name:'Сохранить настройки'}))
  await screen.findByText('Параметры хранения сохранены')
  expect(savedStorage).toEqual({retention_days:30,max_file_bytes:50*1024*1024,max_allowed_bytes:250*1024*1024})
  expect(calls.find(c=>c.path==='/api/v1/admin/settings/storage'&&c.method==='PATCH')?.authorization).toBe('Bearer test-token')
  fireEvent.click(screen.getByRole('switch',{name:'Самостоятельная регистрация'}))
  await waitFor(()=>expect(screen.getByRole('switch').getAttribute('aria-checked')).toBe('true'))
  fireEvent.click(screen.getAllByRole('button',{name:'Заблокировать'}).find(el=>!(el as HTMLButtonElement).disabled)!)
  await screen.findByRole('button',{name:'Восстановить'})
})

it('rejects a non-admin account and does not show controls',async()=>{
  vi.stubGlobal('fetch',vi.fn(async(path:string)=>path==='/api/v1/auth/login'?response({token:'user-token',user:{id:'user',login:'member',role:'user'}}):response(undefined,204)))
  render(<App/>)
  fireEvent.change(screen.getByRole('textbox',{name:'Логин'}),{target:{value:'member'}})
  fireEvent.change(screen.getByLabelText('Пароль'),{target:{value:'password'}})
  fireEvent.click(screen.getByRole('button',{name:'Войти'}))
  await screen.findByRole('alert')
  expect(screen.queryByRole('switch')).toBeNull()
})

it('ignores a late list response after logout',async()=>{
  const user={id:'admin',login:'mother',role:'admin'}
  let finishList!:(value:unknown)=>void
  const list=new Promise<unknown>(resolve=>{finishList=resolve})
  vi.stubGlobal('fetch',vi.fn(async(path:string)=>{
    if(path==='/api/v1/auth/login')return response({token:'test-token',user})
    if(path.startsWith('/api/v1/admin/users?'))return await list
    if(path==='/api/v1/admin/settings/registration')return response({enabled:false})
    if(path==='/api/v1/admin/settings/storage')return response({retention_days:0,max_file_bytes:250*1024*1024,max_allowed_bytes:250*1024*1024})
    return response(undefined,204)
  }))
  render(<App/>)
  fireEvent.change(screen.getByRole('textbox',{name:'Логин'}),{target:{value:'mother'}})
  fireEvent.change(screen.getByLabelText('Пароль'),{target:{value:'password'}})
  fireEvent.click(screen.getByRole('button',{name:'Войти'}))
  await screen.findByRole('button',{name:'Выйти'})
  fireEvent.click(screen.getByRole('button',{name:'Выйти'}))
  finishList(response({users:[user],next_cursor:''}))
  await waitFor(()=>expect(screen.getByRole('button',{name:'Войти'})).toBeTruthy())
  expect(screen.queryByRole('heading',{name:'Доступ к чату'})).toBeNull()
})
