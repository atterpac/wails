(()=>{var l=Object.defineProperty;var x=t=>l(t,"__esModule",{value:!0});var h=(t,e)=>{x(t);for(var n in e)l(t,n,{get:e[n],enumerable:!0})};var p={};h(p,{LogDebug:()=>M,LogError:()=>R,LogFatal:()=>I,LogInfo:()=>N,LogLevel:()=>A,LogPrint:()=>y,LogTrace:()=>m,LogWarning:()=>S,SetLogLevel:()=>P});var f=[];function s(t){if(window.chrome.webview.postMessage(t),f.length>0)for(let e=0;e<f.length;e++)f[e](t)}function i(t,e){s("L"+t+e)}function m(t){i("T",t)}function y(t){i("P",t)}function M(t){i("D",t)}function N(t){i("I",t)}function S(t){i("W",t)}function R(t){i("E",t)}function I(t){i("F",t)}function P(t){i("S",t)}var A={TRACE:1,DEBUG:2,INFO:3,WARNING:4,ERROR:5};var u=class{constructor(e,n){n=n||-1,this.Callback=o=>(e.apply(null,o),n===-1?!1:(n-=1,n===0))}},r={};function c(t,e,n){r[t]=r[t]||[];let o=new u(e,n);r[t].push(o)}function E(t,e){c(t,e,-1)}function d(t,e){c(t,e,1)}function g(t){let e=t.name;if(r[e]){let n=r[e].slice();for(let o=0;o<r[e].length;o+=1){let v=r[e][o],O=t.data;v.Callback(O)&&n.splice(o,1)}r[e]=n}}function a(t){let e;try{e=JSON.parse(t)}catch(n){let o="Invalid JSON passed to Notify: "+t;throw new Error(o)}g(e)}function L(t){let e={name:t,data:[].slice.apply(arguments).slice(1)};g(e),s("EE"+JSON.stringify(e))}function w(t){s("EX"+t)}window.backend={};window.runtime={...p,EventsOn:E,EventsOnce:d,EventsOnMultiple:c,EventsEmit:L,EventsOff:w};window.wails={_:{EventsNotify:a}};})();
