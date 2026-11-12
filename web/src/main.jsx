import React from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App.jsx'
import { initToken } from './api.js'
import './styles.css'
import './console-shell.css'

initToken()
createRoot(document.getElementById('root')).render(<App />)
