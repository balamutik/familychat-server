export type User = {id:string,login:string,role:'admin'|'user',disabled:boolean,created_at:string}
export type Page = {users:User[],next_cursor:string}
export type Login = {token:string,user:User}

export class APIError extends Error { constructor(public status:number,public code:string) { super(code) } }

export async function request<T>(path:string,method:string,token:string,body?:unknown,signal?:AbortSignal):Promise<T> {
  if (!path.startsWith('/api/v1/')) throw new Error('Invalid API path')
  const response=await fetch(path,{method,headers:{'Content-Type':'application/json',...(token?{Authorization:`Bearer ${token}`}:{})},body:body===undefined?undefined:JSON.stringify(body),signal,credentials:'omit',cache:'no-store'})
  if (!response.ok) { const error=await response.json().catch(()=>({error:'request_failed'})); throw new APIError(response.status,error.error||'request_failed') }
  return response.status===204?undefined as T:await response.json() as T
}
