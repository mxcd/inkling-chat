import { createApp } from 'vue'
import '@fontsource-variable/figtree'
import '@fontsource/gabarito/600.css'
import '@fontsource/gabarito/700.css'
import './styles.css'
import './dark.css'
import './theme' // sets data-theme before the first render
import App from './App.vue'

createApp(App).mount('#app')
