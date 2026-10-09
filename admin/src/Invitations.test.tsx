// @vitest-environment jsdom
import {afterEach,expect,it,vi} from 'vitest'
import {cleanup,fireEvent,render,screen} from '@testing-library/react'
import {Invitations} from './Invitations'
afterEach(()=>{cleanup();vi.unstubAllGlobals()})
it('creates with a selected expiry and revokes an invitation',async()=>{
 let sent:any;let revoked=false
 const invite={id:'one',server_name:'Семья',server_url:'https://chat.example',url:'https://chat.example/invite/abc',expires_at:new Date(Date.now()+86400000).toISOString(),revoked_at:null}
 vi.stubGlobal('fetch',vi.fn(async(_path:string,init:RequestInit)=>{
  expect(new Headers(init.headers).get('Authorization')).toBe('Bearer admin')
  if(init.method==='POST'){sent=JSON.parse(String(init.body));return {ok:true,status:201,json:async()=>invite}}
  if(init.method==='DELETE'){revoked=true;return {ok:true,status:204}}
  return {ok:true,status:200,json:async()=>({invitations:[]})}
 }))
 render(<Invitations token="admin"/>)
 await screen.findByText('Приглашений пока нет.')
 fireEvent.change(screen.getByLabelText('Название сервера'),{target:{value:'Семья'}})
 fireEvent.change(screen.getByLabelText('Адрес сервера'),{target:{value:'https://chat.example'}})
 const date=new Date(Date.now()+3*86400000);date.setMinutes(30,0,0)
 const chosen=new Date(date.getTime()-date.getTimezoneOffset()*60000).toISOString().slice(0,16)
 fireEvent.input(screen.getByLabelText('Действует до'),{target:{value:chosen}})
 fireEvent.click(screen.getByRole('button',{name:'Создать приглашение'}))
 await screen.findByDisplayValue(invite.url)
 expect(sent.expires_at).toBe(new Date(chosen).toISOString())
 fireEvent.click(screen.getByRole('button',{name:'Отозвать'}))
 await screen.findByText('Отозвано')
 expect(revoked).toBe(true)
})
